package coordinator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	ErrLeaderNotHeld = errors.New("lease is held by another active node")
	ErrLeaseConflict = errors.New("concurrent update conflict on lease resource")
)

// LeaderElector abstracts cluster leader election mechanisms.
type LeaderElector interface {
	Name() string
	TryAcquire(ctx context.Context, lockID string, ttl time.Duration) (acquired bool, release func(), err error)
}

// ---------------------------------------------------------------------------
// 1. Standalone / Local In-Memory Elector (for tests and standalone deployments)
// ---------------------------------------------------------------------------

type StandaloneElector struct {
	nodeID    string
	mu        sync.Mutex
	leader    string
	expiresAt time.Time
}

func NewStandaloneElector(nodeID string) *StandaloneElector {
	return &StandaloneElector{nodeID: nodeID}
}

func (e *StandaloneElector) Name() string { return "standalone" }

func (e *StandaloneElector) TryAcquire(ctx context.Context, lockID string, ttl time.Duration) (bool, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	if e.leader != "" && e.leader != e.nodeID && now.Before(e.expiresAt) {
		return false, func() {}, nil
	}

	e.leader = e.nodeID
	e.expiresAt = now.Add(ttl)

	released := false
	release := func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if !released && e.leader == e.nodeID {
			e.leader = ""
			e.expiresAt = time.Time{}
			released = true
		}
	}

	return true, release, nil
}

// ---------------------------------------------------------------------------
// 2. Kubernetes Lease Elector (coordination.k8s.io/v1)
// ---------------------------------------------------------------------------

type KubeLease struct {
	ApiVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   KubeMetadata  `json:"metadata"`
	Spec       KubeLeaseSpec `json:"spec"`
}

type KubeMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type KubeLeaseSpec struct {
	HolderIdentity       string    `json:"holderIdentity"`
	LeaseDurationSeconds int       `json:"leaseDurationSeconds"`
	RenewTime            MicroTime `json:"renewTime"`
	AcquireTime          MicroTime `json:"acquireTime"`
}

// MicroTime is the Kubernetes MicroTime wire format: RFC 3339 with exactly
// six fractional digits. The API server rejects Go's default nanosecond form.
type MicroTime struct{ time.Time }

const microTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

func (t MicroTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + t.UTC().Format(microTimeLayout) + `"`), nil
}

func (t *MicroTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

type KubernetesLeaseElector struct {
	nodeID     string
	namespace  string
	apiBaseURL string
	httpClient *http.Client
	token      string
	tokenFile  string // re-read per call: projected service-account tokens rotate
}

// NewKubernetesLeaseElector constructs an in-cluster or configured Kubernetes Lease coordinator.
func NewKubernetesLeaseElector(nodeID, namespace, apiServerURL, token string) (*KubernetesLeaseElector, error) {
	if namespace == "" {
		if nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			namespace = string(bytes.TrimSpace(nsBytes))
		} else if envNS := os.Getenv("POD_NAMESPACE"); envNS != "" {
			namespace = envNS
		} else {
			namespace = "default"
		}
	}

	tokenFile := ""
	if token == "" {
		if tokBytes, err := os.ReadFile(serviceAccountTokenFile); err == nil {
			token = string(bytes.TrimSpace(tokBytes))
			tokenFile = serviceAccountTokenFile
		}
	}

	if apiServerURL == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host != "" && port != "" {
			apiServerURL = fmt.Sprintf("https://%s:%s", host, port)
		} else {
			apiServerURL = "https://kubernetes.default.svc"
		}
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if caCert, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caCert)
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		}
	}

	return &KubernetesLeaseElector{
		nodeID:     nodeID,
		namespace:  namespace,
		apiBaseURL: strings.TrimRight(apiServerURL, "/"),
		httpClient: client,
		token:      token,
		tokenFile:  tokenFile,
	}, nil
}

const serviceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// authorize sets the bearer token, re-reading the projected service-account
// token file so the elector keeps working after the kubelet rotates it.
func (e *KubernetesLeaseElector) authorize(req *http.Request) {
	tok := e.token
	if e.tokenFile != "" {
		if b, err := os.ReadFile(e.tokenFile); err == nil {
			tok = string(bytes.TrimSpace(b))
		}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
}

func (e *KubernetesLeaseElector) Name() string { return "kubernetes_lease" }

func (e *KubernetesLeaseElector) SetHTTPClient(client *http.Client) {
	e.httpClient = client
}

func (e *KubernetesLeaseElector) TryAcquire(ctx context.Context, lockID string, ttl time.Duration) (bool, func(), error) {
	leaseName := "relayops-" + strings.ToLower(regexpClean(lockID))
	url := fmt.Sprintf("%s/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", e.apiBaseURL, e.namespace, leaseName)

	now := time.Now().UTC()
	ttlSec := int(ttl.Seconds())
	if ttlSec <= 0 {
		ttlSec = 15
	}

	// 1. Fetch current Lease object
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, nil, err
	}
	e.authorize(req)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false, nil, fmt.Errorf("k8s api call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 2. Create Lease
		lease := KubeLease{
			ApiVersion: "coordination.k8s.io/v1",
			Kind:       "Lease",
			Metadata:   KubeMetadata{Name: leaseName, Namespace: e.namespace},
			Spec: KubeLeaseSpec{
				HolderIdentity:       e.nodeID,
				LeaseDurationSeconds: ttlSec,
				RenewTime:            MicroTime{now},
				AcquireTime:          MicroTime{now},
			},
		}
		body, _ := json.Marshal(lease)
		postURL := fmt.Sprintf("%s/apis/coordination.k8s.io/v1/namespaces/%s/leases", e.apiBaseURL, e.namespace)
		pReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, postURL, bytes.NewReader(body))
		pReq.Header.Set("Content-Type", "application/json")
		e.authorize(pReq)
		pResp, pErr := e.httpClient.Do(pReq)
		if pErr != nil {
			return false, nil, pErr
		}
		defer pResp.Body.Close()
		if pResp.StatusCode == http.StatusCreated {
			return true, e.makeRelease(leaseName), nil
		}
		return false, nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return false, nil, fmt.Errorf("unexpected k8s lease status %d", resp.StatusCode)
	}

	var existing KubeLease
	if err := json.NewDecoder(resp.Body).Decode(&existing); err != nil {
		return false, nil, err
	}

	// Check if active lease held by another
	leaseExpires := existing.Spec.RenewTime.Time.Add(time.Duration(existing.Spec.LeaseDurationSeconds) * time.Second)
	isCurrentLeader := existing.Spec.HolderIdentity == e.nodeID
	isExpired := now.After(leaseExpires)
	isVacant := existing.Spec.HolderIdentity == ""

	if !isCurrentLeader && !isExpired && !isVacant {
		return false, nil, nil
	}

	// 3. Update Lease (renew or takeover)
	acquireTime := existing.Spec.AcquireTime
	if !isCurrentLeader {
		acquireTime = MicroTime{now}
	}
	existing.Spec.HolderIdentity = e.nodeID
	existing.Spec.LeaseDurationSeconds = ttlSec
	existing.Spec.RenewTime = MicroTime{now}
	existing.Spec.AcquireTime = acquireTime

	putBody, _ := json.Marshal(existing)
	putReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(putBody))
	putReq.Header.Set("Content-Type", "application/json")
	e.authorize(putReq)
	putResp, putErr := e.httpClient.Do(putReq)
	if putErr != nil {
		return false, nil, putErr
	}
	defer putResp.Body.Close()

	if putResp.StatusCode == http.StatusOK {
		return true, e.makeRelease(leaseName), nil
	}

	return false, nil, nil
}

func (e *KubernetesLeaseElector) makeRelease(leaseName string) func() {
	return func() {
		// Clean release: zero out holderIdentity
		url := fmt.Sprintf("%s/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s", e.apiBaseURL, e.namespace, leaseName)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		e.authorize(req)
		resp, err := e.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			return
		}
		var l KubeLease
		_ = json.NewDecoder(resp.Body).Decode(&l)
		resp.Body.Close()

		if l.Spec.HolderIdentity == e.nodeID {
			l.Spec.HolderIdentity = ""
			body, _ := json.Marshal(l)
			pReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
			pReq.Header.Set("Content-Type", "application/json")
			e.authorize(pReq)
			pResp, pErr := e.httpClient.Do(pReq)
			if pErr == nil {
				pResp.Body.Close()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 3. PostgreSQL Advisory Lock Adapter
// ---------------------------------------------------------------------------

type AdvisoryLockStore interface {
	WithAdvisoryLock(ctx context.Context, lockKey int64, fn func(ctx context.Context) error) (bool, error)
}

type PostgresAdvisoryElector struct {
	store AdvisoryLockStore
}

func NewPostgresAdvisoryElector(store AdvisoryLockStore) *PostgresAdvisoryElector {
	return &PostgresAdvisoryElector{store: store}
}

func (e *PostgresAdvisoryElector) Name() string { return "postgres_advisory" }

func (e *PostgresAdvisoryElector) TryAcquire(ctx context.Context, lockID string, ttl time.Duration) (bool, func(), error) {
	if e.store == nil {
		return false, nil, errors.New("postgres store not configured")
	}

	h := fnv.New64a()
	h.Write([]byte(lockID))
	key := int64(h.Sum64())

	var innerRelease func()
	acquired, err := e.store.WithAdvisoryLock(ctx, key, func(lockedCtx context.Context) error {
		innerRelease = func() {}
		return nil
	})
	return acquired, innerRelease, err
}

func regexpClean(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + ('a' - 'A'))
		}
	}
	if b.Len() == 0 {
		return "leader"
	}
	return b.String()
}
