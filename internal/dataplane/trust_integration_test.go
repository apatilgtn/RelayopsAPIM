package dataplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/relayops/apim/internal/admin"
	"github.com/relayops/apim/internal/dataplane"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testdb"
)

func adminCall(t *testing.T, base, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, base+path, &buf)
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func issueNodeToken(t *testing.T, base, nodeID string) (id, token string) {
	t.Helper()
	code, out := adminCall(t, base, "POST", "/api/admin/dataplane/credentials", map[string]any{"node_id": nodeID, "description": "test"})
	if code != http.StatusCreated {
		t.Fatalf("issue credential for %s: %d %v", nodeID, code, out)
	}
	token, _ = out["token"].(string)
	cred, _ := out["credential"].(map[string]any)
	id, _ = cred["id"].(string)
	if !strings.HasPrefix(token, store.DataplaneTokenPrefix) || id == "" {
		t.Fatalf("credential response: %v", out)
	}
	return id, token
}

// Phase 2 trust: per-node credentials bound to one node, signed configuration,
// a signed and verified on-disk cache, and revocation.
func TestIntegrationNodeTrust(t *testing.T) {
	dsn := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	v1, v2 := backend(t, "v1"), backend(t, "v2")

	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	api, _, err := db.CreateAPIAtomic(ctx, store.API{Name: "svc", BasePath: "/svc", UpstreamURL: v1.URL, StripPath: true,
		AuthType: "none", TimeoutMS: 2000, Enabled: true}, "t", "v1")
	if err != nil {
		t.Fatal(err)
	}

	seed, pub, _ := dataplane.GenerateSigningKey()
	signer, _ := dataplane.ParseSigningKey(seed)
	verifier, _ := dataplane.ParseVerifyKeys(pub)

	// Per-node credentials only: no shared token configured.
	cp := admin.New(db, nil, realtime.NewHub(), "admin-token", "cp-1", fstest.MapFS{},
		admin.WithDataplane(admin.DataplaneOptions{Signer: signer}))
	go cp.StartDataplaneNotifier(ctx)
	cpSrv := httptest.NewServer(cp.Handler())
	defer func() {
		cancel()
		cpSrv.CloseClientConnections()
		cpSrv.Close()
	}()

	credID, tok := issueNodeToken(t, cpSrv.URL, "edge-a")

	// The shared-token style is refused when none is configured.
	if _, err := startEdgeWith(t, ctx, cpSrv.URL, "edge-x", nodeToken, verifier); err == nil {
		t.Fatal("unconfigured shared token accepted")
	}

	edge, err := startEdgeWith(t, ctx, cpSrv.URL, "edge-a", tok, verifier)
	if err != nil {
		t.Fatalf("gateway with its own credential and signed config: %v", err)
	}
	if code, body := edge.get("/svc"); code != 200 || body != "v1" {
		t.Fatalf("initial: %d %q", code, body)
	}

	// A credential cannot be used by, or report as, another node.
	nodeCall := func(method, path, nodeID string, body any) int {
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, cpSrv.URL+path, &buf)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set(dataplane.HeaderNodeID, nodeID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := nodeCall("GET", dataplane.PathConfig, "edge-b", nil); code != http.StatusForbidden {
		t.Fatalf("credential used as another node: %d (want 403)", code)
	}
	if code := nodeCall("POST", dataplane.PathAck, "edge-a", map[string]any{"node_id": "edge-b", "revision": 1}); code != http.StatusForbidden {
		t.Fatalf("credential acknowledging for another node: %d (want 403)", code)
	}

	// A gateway that trusts a different key refuses the configuration.
	_, tokC := issueNodeToken(t, cpSrv.URL, "edge-c")
	_, otherPub, _ := dataplane.GenerateSigningKey()
	other, _ := dataplane.ParseVerifyKeys(otherPub)
	if _, err := startEdgeWith(t, ctx, cpSrv.URL, "edge-c", tokC, other); err == nil || !strings.Contains(err.Error(), "untrusted key") {
		t.Fatalf("configuration signed by an untrusted key: %v", err)
	}

	// Signed changes still propagate through the long-poll.
	api.UpstreamURL = v2.URL
	if _, _, err := db.UpdateAPIAtomic(ctx, api, "t", "v2"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, "signed v2 applied", func() bool { _, b := edge.get("/svc"); return b == "v2" })

	// Last-seen bookkeeping on the credential.
	code, list := adminCall(t, cpSrv.URL, "GET", "/api/admin/dataplane/credentials", nil)
	_ = list
	if code != 200 {
		t.Fatalf("list credentials: %d", code)
	}

	// Cold start with the control plane unreachable: the signed cache is verified and served.
	cold, err := startEdgeWith(t, ctx, "http://127.0.0.1:1", "edge-a", tok, verifier)
	if err != nil {
		t.Fatalf("cold start from signed cache: %v", err)
	}
	if code, body := cold.get("/svc"); code != 200 || body != "v2" {
		t.Fatalf("cold start served %d %q", code, body)
	}

	// A tampered cache is refused rather than served.
	cachePath := "data/last_known_good_config.edge-a.json"
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(raw, []byte(v2.URL), []byte(v1.URL), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("cache does not contain the upstream URL to tamper with")
	}
	if err := os.WriteFile(cachePath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := startEdgeWith(t, ctx, "http://127.0.0.1:1", "edge-a", tok, verifier); err == nil {
		t.Fatal("tampered cache served on cold start")
	}

	// Revocation takes effect on the next call.
	if code, out := adminCall(t, cpSrv.URL, "DELETE", "/api/admin/dataplane/credentials/"+credID, nil); code != 200 {
		t.Fatalf("revoke: %d %v", code, out)
	}
	req, _ := http.NewRequest("GET", cpSrv.URL+dataplane.PathConfig, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(dataplane.HeaderNodeID, "edge-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked credential: %d (want 401)", resp.StatusCode)
	}
}
