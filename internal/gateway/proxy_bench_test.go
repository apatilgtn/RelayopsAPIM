package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// BenchmarkProxyOpen measures the gateway's own cost per proxied request
// (routing, policy, decision trail, logging, reverse proxy) against a
// local upstream.
func BenchmarkProxyOpen(b *testing.B) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"items":[1,2,3]}`)
	}))
	defer up.Close()
	g := newTestGatewayB(b)
	g.loadB(b, store.SnapshotData{APIs: []store.API{testAPI("open", "/open", up.URL)}})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest("GET", "/open/items?x=1", nil)
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			if rec.Code != 200 {
				b.Fatalf("status %d", rec.Code)
			}
		}
	})
}

func newTestGatewayB(b *testing.B) *Gateway {
	b.Helper()
	return New(analytics.NewCollector(nil, realtime.NewHub(), "bench"), nil, "bench")
}

func (g *Gateway) loadB(b *testing.B, d store.SnapshotData) {
	b.Helper()
	snap, errs := buildSnapshot(d, 10, g.upstreams)
	if len(errs) > 0 {
		b.Fatal(errs)
	}
	g.swap(snap)
}
