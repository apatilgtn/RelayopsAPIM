package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// recoveryModeServer is a control plane started without a database
// (cached-recovery mode): the gateway serves its last-known-good config.
func recoveryModeServer(t *testing.T) http.Handler {
	t.Helper()
	gw := gateway.New(analytics.NewCollector(nil, realtime.NewHub(), "n"), nil, "n")
	return New(nil, gw, realtime.NewHub(), "tok", "n", fstest.MapFS{}).Handler()
}

func serve(h http.Handler, method, path, token string) (rec *httptest.ResponseRecorder, panicked any) {
	defer func() { panicked = recover() }()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, nil
}

func TestRecoveryModeNeverPanics(t *testing.T) {
	h := recoveryModeServer(t)

	rec, p := serve(h, "GET", "/metrics", "")
	if p != nil || rec.Code != 200 || !strings.Contains(rec.Body.String(), "relayops_requests_total") {
		t.Fatalf("/metrics in recovery mode: panic=%v code=%d", p, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "relayops_control_plane_database_up 0") {
		t.Fatalf("/metrics must report the database as down:\n%s", rec.Body)
	}

	rec, p = serve(h, "GET", "/healthz", "")
	if p != nil || !strings.Contains(rec.Body.String(), `"degraded"`) {
		t.Fatalf("/healthz: panic=%v body=%s", p, rec.Body)
	}

	// Every admin and portal route answers 503 database_unavailable instead of crashing.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/overview"}, {"GET", "/api/apis"}, {"POST", "/api/apis"}, {"GET", "/api/fleet/status"},
		{"GET", "/api/revisions"}, {"POST", "/api/system/plan"}, {"GET", "/api/revisions/auto-rollback/config"},
		{"POST", "/api/revisions/auto-rollback/evaluate"}, {"GET", "/api/logs"}, {"GET", "/api/system/drift"},
		{"GET", "/portal/api/catalog"}, {"POST", "/portal/api/register"},
	} {
		rec, p := serve(h, tc.method, tc.path, "tok")
		if p != nil {
			t.Fatalf("%s %s panicked: %v", tc.method, tc.path, p)
		}
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "database_unavailable") {
			t.Fatalf("%s %s = %d %s, want 503 database_unavailable", tc.method, tc.path, rec.Code, rec.Body)
		}
	}

	// A wrong token is still rejected; the outage must not open the control plane.
	if rec, p := serve(h, "GET", "/api/apis", "wrong"); p != nil || rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token in recovery mode: panic=%v code=%d", p, rec.Code)
	}
	// Local node status still works with the cluster token.
	if rec, p := serve(h, "GET", "/api/upstreams/health", "tok"); p != nil || rec.Code != 200 {
		t.Fatalf("upstream health in recovery mode: panic=%v code=%d", p, rec.Code)
	}
}

func TestMetricsWithoutGateway(t *testing.T) {
	h := New(nil, nil, nil, "tok", "n", fstest.MapFS{}).Handler()
	if rec, p := serve(h, "GET", "/metrics", ""); p != nil || rec.Code != 200 {
		t.Fatalf("metrics without gateway: panic=%v code=%d", p, rec.Code)
	}
}

// When the database becomes unreachable after startup, metrics must still
// answer quickly rather than hang the scrape on database timeouts.
func TestMetricsBoundedWhenDatabaseUnreachable(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, "postgres://postgres@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err == nil {
		s.Close()
		t.Skip("unexpectedly connected")
	}
	// Open fails fast for an unreachable server; model a store whose pool cannot connect.
	pool, _ := newUnreachablePool(ctx)
	if pool == nil {
		t.Skip("could not build an unreachable pool")
	}
	gw := gateway.New(analytics.NewCollector(nil, realtime.NewHub(), "n"), nil, "n")
	h := New(&store.Store{Pool: pool}, gw, realtime.NewHub(), "tok", "n", fstest.MapFS{}).Handler()
	start := time.Now()
	rec, p := serve(h, "GET", "/metrics", "")
	if p != nil || rec.Code != 200 || time.Since(start) > 4*time.Second {
		t.Fatalf("metrics with unreachable DB: panic=%v code=%d took=%s", p, rec.Code, time.Since(start))
	}
	if !strings.Contains(rec.Body.String(), "relayops_control_plane_database_up 0") {
		t.Fatalf("database_up should be 0:\n%s", rec.Body)
	}
}

func newUnreachablePool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig("postgres://postgres@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, cfg) // connects lazily, so creation succeeds
}
