// Package dataplane connects gateway-only nodes (RELAYOPS_ROLE=gateway) to the
// control plane's node API. Such nodes hold no database credentials: they pull
// configuration, report acknowledgements, ship request logs and settle AI
// budgets over HTTPS, authenticated with the shared node token.
//
// The control-plane side of the protocol lives in internal/admin/dataplane.go;
// the wire types shared by both sides are defined here.
package dataplane

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
)

// API paths served by the control plane.
const (
	PathConfig        = "/dataplane/v1/config"
	PathAck           = "/dataplane/v1/ack"
	PathLogs          = "/dataplane/v1/logs"
	PathBudgetLookup  = "/dataplane/v1/ai-budget/lookup"
	PathBudgetReserve = "/dataplane/v1/ai-budget/reserve"
	PathBudgetSettle  = "/dataplane/v1/ai-budget/settle"
	PathBudgetRelease = "/dataplane/v1/ai-budget/release"
	PathWhoami        = "/dataplane/v1/whoami"
)

// Node identity headers sent with every request.
const (
	HeaderNodeID    = "X-RelayOps-Node-Id"
	HeaderNodeGroup = "X-RelayOps-Node-Group"
	HeaderCanary    = "X-RelayOps-Node-Canary"
)

// MaxWait caps how long the control plane holds a config long-poll open.
const MaxWait = 30 * time.Second

// ConfigResponse is the body of GET /dataplane/v1/config: a ConfigPayload as
// raw JSON, its fingerprint (hex SHA-256 of the payload bytes) and, when the
// control plane has a signing key, an Ed25519 signature over the fingerprint
// and issue time.
type ConfigResponse struct {
	Fingerprint string          `json:"fingerprint"`
	IssuedAt    time.Time       `json:"issued_at"`
	Payload     json.RawMessage `json:"payload"`
	KeyID       string          `json:"key_id,omitempty"`
	Signature   string          `json:"signature,omitempty"`
}

// Budget request bodies.
type (
	BudgetLookupRequest struct {
		TenantID   string `json:"tenant_id"`
		ConsumerID string `json:"consumer_id"`
	}
	BudgetReserveRequest struct {
		AccountID    string `json:"account_id"`
		RequestID    string `json:"request_id"`
		ReserveCents int64  `json:"reserve_cents"`
	}
	BudgetSettleRequest struct {
		RequestID string        `json:"request_id"`
		Usage     store.AIUsage `json:"usage"`
	}
	BudgetSettleResponse struct {
		Cents int64 `json:"cents"`
	}
	BudgetReleaseRequest struct {
		RequestID string `json:"request_id"`
	}
)

// Error codes the node API returns in {"error": code} bodies, mapped back to
// the store's sentinel errors so gateway behaviour matches combined mode.
const (
	CodeNotFound       = "not_found"
	CodeBudgetExceeded = "budget_exceeded"
)

// ClientConfig identifies this node to the control plane.
type ClientConfig struct {
	BaseURL   string
	Token     string
	NodeID    string
	NodeGroup string
	IsCanary  bool
	AllowHTTP bool        // plain-HTTP control plane (local development only)
	TLS       *tls.Config // optional: private CA and/or client certificate (mTLS)
}

// LoadClientTLS builds the TLS settings for calling the control plane: a CA
// bundle to trust (instead of the system roots) and a client certificate for
// mTLS. Empty paths are skipped; it returns nil when all are empty.
func LoadClientTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	if caFile == "" && certFile == "" && keyFile == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read control plane CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		cfg.RootCAs = pool
	}
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load node client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// Client calls the control plane's node API.
type Client struct {
	base string
	cfg  ClientConfig
	http *http.Client
}

func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid control plane URL %q", cfg.BaseURL)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowHTTP {
			return nil, errors.New("control plane URL must use https (set RELAYOPS_DATAPLANE_ALLOW_HTTP=true for local development)")
		}
	default:
		return nil, fmt.Errorf("unsupported control plane URL scheme %q", u.Scheme)
	}
	if cfg.Token == "" {
		return nil, errors.New("node API token is required")
	}
	return &Client{
		base: strings.TrimRight(cfg.BaseURL, "/"),
		cfg:  cfg,
		// No client-wide timeout: long-polls set their own deadline per request.
		http: &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     cfg.TLS,
		}},
	}, nil
}

// forGroup returns a client that asks for another node group's configuration,
// sharing this client's credentials and connections (used by relays).
func (c *Client) forGroup(group string, canary bool) *Client {
	cp := *c
	cp.cfg.NodeGroup, cp.cfg.IsCanary = group, canary
	return &cp
}

// StatusError is a non-2xx response from the control plane.
type StatusError struct {
	Status int
	Code   string
}

func (e *StatusError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("control plane returned %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("control plane returned %d", e.Status)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set(HeaderNodeID, c.cfg.NodeID)
	req.Header.Set(HeaderNodeGroup, c.cfg.NodeGroup)
	req.Header.Set(HeaderCanary, strconv.FormatBool(c.cfg.IsCanary))
	return req, nil
}

// postJSON sends in as JSON (gzip-compressed when gz is set) and decodes a 2xx
// response into out when out is non-nil.
func (c *Client) postJSON(ctx context.Context, path string, in, out any, gz bool) error {
	var buf bytes.Buffer
	if gz {
		zw := gzip.NewWriter(&buf)
		if err := json.NewEncoder(zw).Encode(in); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
	} else if err := json.NewEncoder(&buf).Encode(in); err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func statusError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	return &StatusError{Status: resp.StatusCode, Code: body.Error}
}

// fetchConfig fetches configuration. With etag set and wait > 0 it long-polls:
// it returns (nil, nil) when nothing changed before the wait elapsed.
func (c *Client) fetchConfig(ctx context.Context, etag string, wait time.Duration) (*fetchedConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, wait+20*time.Second)
	defer cancel()
	path := PathConfig
	if wait > 0 {
		path += "?wait=" + strconv.Itoa(int(wait/time.Second))
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, nil
	case http.StatusOK:
	default:
		return nil, statusError(resp)
	}
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxConfigBytes))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return parseEnvelope(raw)
}

// maxConfigBytes bounds a decompressed configuration response.
const maxConfigBytes = 256 << 20

// fetchedConfig is a configuration envelope plus its raw bytes, which are what
// the signed on-disk cache stores.
type fetchedConfig struct {
	*ConfigResponse
	raw []byte
}

func parseEnvelope(raw []byte) (*fetchedConfig, error) {
	var out ConfigResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if out.Fingerprint == "" || len(out.Payload) == 0 {
		return nil, errors.New("config response has no fingerprint or payload")
	}
	return &fetchedConfig{ConfigResponse: &out, raw: raw}, nil
}
