package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestIntegrationMCPCatalogLifecycle(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	api, _, err := s.CreateAPIAtomic(ctx, API{Name: "mcp-cat", BasePath: "/mcp-cat", UpstreamURL: "http://127.0.0.1:9",
		AuthType: "none", TimeoutMS: 1000, Enabled: true, Protocol: "mcp"}, "t", "mcp")
	if err != nil {
		t.Fatal(err)
	}
	fp := "sha256:" + strings.Repeat("b", 64)
	obs := MCPObservation{APIID: api.ID, Kind: "tool", Name: "search", Fingerprint: fp, Definition: json.RawMessage(`{"name":"search"}`)}

	// Recorded for information first (no approved catalog yet).
	info := obs
	info.Status = "resolved"
	created, err := s.RecordMCPObservations(ctx, []MCPObservation{info}, "discovery")
	if err != nil || len(created) != 1 || created[0].Status != "resolved" {
		t.Fatalf("first record: %+v %v", created, err)
	}

	// Listen for reloads: refreshing a known definition must not trigger one.
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN relayops_config"); err != nil {
		t.Fatal(err)
	}
	notified := func() bool {
		wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, err := conn.Conn().WaitForNotification(wctx)
		return err == nil
	}
	if created, err = s.RecordMCPObservations(ctx, []MCPObservation{info}, "node-a"); err != nil || len(created) != 0 {
		t.Fatalf("refresh: %+v %v", created, err)
	}
	if notified() {
		t.Fatal("a last_seen refresh triggered a gateway reload")
	}

	// Once the API has a catalog, the gateway reports it as pending: the
	// informational entry is promoted (and reloads gateways).
	pending := obs
	pending.Status = "pending"
	created, err = s.RecordMCPObservations(ctx, []MCPObservation{pending}, "node-a")
	if err != nil || len(created) != 1 || created[0].Status != "pending" {
		t.Fatalf("promotion: %+v %v", created, err)
	}
	if !notified() {
		t.Fatal("promotion to pending did not reload gateways")
	}

	// A person's decision sticks: a resolved entry with a decider is not
	// promoted again.
	if err := s.SetMCPCatalogStatus(ctx, created[0].ID, "resolved", "alice"); err != nil {
		t.Fatal(err)
	}
	if created, err = s.RecordMCPObservations(ctx, []MCPObservation{pending}, "node-a"); err != nil || len(created) != 0 {
		t.Fatalf("decided entry re-opened: %+v %v", created, err)
	}

	// Pins sync approvals; the snapshot carries pending and approved state.
	if err := s.SyncMCPApprovals(ctx, api.ID, "tool", map[string]string{"search": fp}, "bob"); err != nil {
		t.Fatal(err)
	}
	e, _ := s.GetMCPCatalogEntry(ctx, created0(t, s, api.ID))
	if e.Status != "approved" || e.DecidedBy != "bob" {
		t.Fatalf("synced entry %+v", e)
	}
}

func created0(t *testing.T, s *Store, apiID string) string {
	t.Helper()
	list, err := s.ListMCPCatalog(context.Background(), nil, apiID, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v %v", list, err)
	}
	return list[0].ID
}
