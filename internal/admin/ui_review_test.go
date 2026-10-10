package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/gateway"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// Opt-in browser review against throwaway databases; never changes the product database.
// RELAYOPS_UI_REVIEW=1 go test ./internal/admin -run TestUIReviewServer -timeout 30m
func TestUIReviewServer(t *testing.T) {
	if os.Getenv("RELAYOPS_UI_REVIEW") != "1" {
		t.Skip("interactive UI review disabled")
	}
	if os.Getenv("RELAYOPS_TEST_DATABASE_URL") == "" {
		t.Setenv("RELAYOPS_TEST_DATABASE_URL", config.Load().DatabaseURL)
	}
	full, empty := openAdminTestStore(t), openAdminTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"source":"isolated-ui-review"}`)
	}))
	defer upstream.Close()
	plan, err := full.CreatePlan(ctx, store.Plan{Name: "Standard", Description: "Default API access", RateLimitPerMinute: 600})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"Orders", "Payments", "AI inference"} {
		spec := map[string]any{"openapi": "3.0.3", "info": map[string]any{"title": name, "version": "1.0"}, "paths": map[string]any{"/status": map[string]any{"get": map[string]any{"summary": "Service status", "responses": map[string]any{"200": map[string]any{"description": "Available"}}}}}}
		a, _, err := full.CreateAPIAtomic(ctx, store.API{Name: name, Description: "Production integration and service status.", BasePath: []string{"/orders", "/payments", "/ai"}[i], UpstreamURL: upstream.URL, StripPath: true, AuthType: "none", Enabled: true, Visibility: "public", TimeoutMS: 5000, IsAI: i == 2, RequireApproval: i == 1, OpenAPISpec: spec}, "UI review", "Add example service")
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			c, e := full.CreateConsumer(ctx, store.Consumer{Name: "Example application", Email: "developer@example.test", Status: "active"})
			if e != nil {
				t.Fatal(e)
			}
			_, e = full.CreateSubscriptionWithStatus(ctx, c.ID, a.ID, &plan.ID, "pending", false)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	requester, err := full.CreateAdminUser(ctx, store.AdminUser{Name: "Access requester", Email: "requester@example.test", Team: "Integrations", Role: "auditor", Active: false, SSOProvider: "local", PasswordHash: ""})
	if err != nil {
		t.Fatal(err)
	}
	requester.Active = false
	_, err = full.UpdateAdminUser(ctx, requester)
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*store.Store{full, empty} {
		user, e := db.GetAdminUserByEmail(ctx, "admin@relayops.local")
		if e != nil {
			t.Fatal(e)
		}
		user.Active = true
		user.Role = "superadmin"
		_, err = db.UpdateAdminUser(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
	}
	hub := realtime.NewHub()
	collector := analytics.NewCollector(full, hub, "ui-review")
	gw := gateway.New(collector, nil, "ui-review")
	watcher := gateway.NewWatcher(gw, full, hub, time.Minute)
	if err := watcher.Reload(ctx, "ui-review"); err != nil {
		t.Fatal(err)
	}
	go watcher.Run(ctx)
	go collector.Run(ctx)
	proxy := httptest.NewServer(gw)
	defer proxy.Close()
	// The same embedded source layout is used by the product; asset serving is separately tested.
	static := os.DirFS("../../web/static")
	stop := make(chan struct{})
	var once sync.Once
	var servers []*http.Server
	for i, db := range []*store.Store{full, empty, nil} {
		core := New(db, gw, hub, "ui-review-token", "ui-review", static, WithGatewayAddress(strings.TrimPrefix(proxy.URL, "http://")))
		base := core.Handler()
		mux := http.NewServeMux()
		mux.HandleFunc("/__review/stop", func(w http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(stop) })
			fmt.Fprint(w, "stopping")
		})
		mux.Handle("/", base)
		port := []int{9196, 9198, 9199}[i]
		ln, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if e != nil {
			t.Fatal(e)
		}
		srv := &http.Server{Handler: mux}
		servers = append(servers, srv)
		go srv.Serve(ln)
	}
	t.Cleanup(func() {
		for _, srv := range servers {
			shutdown, done := context.WithTimeout(context.Background(), time.Second)
			_ = srv.Shutdown(shutdown)
			done()
		}
	})
	marker, _ := json.Marshal(map[string]string{"populated": "http://127.0.0.1:9196", "empty": "http://127.0.0.1:9198", "unavailable": "http://127.0.0.1:9199"})
	t.Log(string(marker))
	select {
	case <-stop:
	case <-time.After(25 * time.Minute):
	}
}
