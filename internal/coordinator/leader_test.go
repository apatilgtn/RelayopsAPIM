package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestStandaloneElector(t *testing.T) {
	e1 := NewStandaloneElector("node-1")
	e2 := &StandaloneElector{nodeID: "node-2"}
	ctx := context.Background()

	// 1. Node 1 acquires
	acq1, rel1, err := e1.TryAcquire(ctx, "test-lock", 2*time.Second)
	if err != nil || !acq1 {
		t.Fatalf("expected node-1 to acquire lease: %v, err: %v", acq1, err)
	}

	// 2. Share state pointer for mutual exclusion test
	e2.leader = e1.leader
	e2.expiresAt = e1.expiresAt

	// Node 2 tries to acquire -> must fail
	acq2, _, err := e2.TryAcquire(ctx, "test-lock", 2*time.Second)
	if err != nil || acq2 {
		t.Fatalf("expected node-2 to be rejected while lease active: %v", acq2)
	}

	// 3. Node 1 releases
	rel1()
	e2.leader = e1.leader
	e2.expiresAt = e1.expiresAt

	// 4. Node 2 can now acquire
	acq2After, rel2, err := e2.TryAcquire(ctx, "test-lock", 2*time.Second)
	if err != nil || !acq2After {
		t.Fatalf("expected node-2 to acquire after release: %v", acq2After)
	}
	rel2()
}

func TestKubernetesLeaseElectorWithMockAPIServer(t *testing.T) {
	var mu sync.Mutex
	leases := make(map[string]KubeLease)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		path := r.URL.Path
		switch r.Method {
		case http.MethodGet:
			l, ok := leases[path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(l)
		case http.MethodPost:
			var l KubeLease
			_ = json.NewDecoder(r.Body).Decode(&l)
			targetPath := path + "/" + l.Metadata.Name
			leases[targetPath] = l
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(l)
		case http.MethodPut:
			var l KubeLease
			_ = json.NewDecoder(r.Body).Decode(&l)
			leases[path] = l
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(l)
		}
	}))
	defer server.Close()

	elector1, err := NewKubernetesLeaseElector("pod-1", "default", server.URL, "token-secret")
	if err != nil {
		t.Fatalf("failed to create elector: %v", err)
	}
	elector1.SetHTTPClient(server.Client())

	elector2, err := NewKubernetesLeaseElector("pod-2", "default", server.URL, "token-secret")
	if err != nil {
		t.Fatalf("failed to create elector 2: %v", err)
	}
	elector2.SetHTTPClient(server.Client())

	ctx := context.Background()

	// 1. Pod 1 acquires initial lease
	acq1, rel1, err := elector1.TryAcquire(ctx, "autorollback", 5*time.Second)
	if err != nil || !acq1 {
		t.Fatalf("pod-1 should acquire fresh lease: %v (err: %v)", acq1, err)
	}

	// 2. Pod 2 tries to acquire active lease -> must fail
	acq2, _, err := elector2.TryAcquire(ctx, "autorollback", 5*time.Second)
	if err != nil || acq2 {
		t.Fatalf("pod-2 should NOT acquire active lease: %v", acq2)
	}

	// 3. Pod 1 renews lease while holding it
	renew1, _, err := elector1.TryAcquire(ctx, "autorollback", 5*time.Second)
	if err != nil || !renew1 {
		t.Fatalf("pod-1 should renew held lease: %v", renew1)
	}

	// 4. Pod 1 releases
	rel1()

	// 5. Pod 2 can now acquire
	acq2After, rel2, err := elector2.TryAcquire(ctx, "autorollback", 5*time.Second)
	if err != nil || !acq2After {
		t.Fatalf("pod-2 should acquire released lease: %v", acq2After)
	}
	rel2()
}

// The Kubernetes API server accepts MicroTime only with six fractional digits.
func TestMicroTimeWireFormat(t *testing.T) {
	ts := MicroTime{time.Date(2026, 10, 9, 2, 44, 47, 393390123, time.UTC)}
	b, err := json.Marshal(ts)
	if err != nil || string(b) != `"2026-10-09T02:44:47.393390Z"` {
		t.Fatalf("marshal: %s %v", b, err)
	}
	var back MicroTime
	if err := json.Unmarshal([]byte(`"2026-10-09T02:44:47.393390Z"`), &back); err != nil || !back.Equal(ts.Truncate(time.Microsecond)) {
		t.Fatalf("unmarshal: %v %v", back, err)
	}
}
