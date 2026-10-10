package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestIntegrationRefusalSummary(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(i int, status int, reason string, evals map[string]any) RequestLog {
		return RequestLog{TS: now, NodeID: "n", RequestID: fmt.Sprint(i), LogID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), APIName: "api",
			Method: "POST", Path: "/x", Status: status, DecisionReason: reason, PolicyEvaluations: evals}
	}
	logs := []RequestLog{
		mk(1, 400, "graphql_policy_violation", map[string]any{"graphql": map[string]any{"depth": 9}}),
		mk(2, 200, "tool_not_pinned", map[string]any{"mcp": map[string]any{"refused": "tool_not_pinned"}}),
		mk(3, 403, "method_not_allowed", map[string]any{"grpc": map[string]any{"method": "Delete"}}),
		mk(4, 403, "ip_not_allowed", map[string]any{"wasm": []any{map[string]any{"plugin": "ip-allowlist"}}}),
		// Not refusals: proxied, upstream failures, a gRPC error from the service.
		mk(5, 200, "proxied_successfully", map[string]any{"mcp": map[string]any{}}),
		mk(6, 504, "upstream_timeout", nil),
		mk(7, 503, "grpc_unavailable", map[string]any{"grpc": map[string]any{}}),
	}
	if err := s.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	counts, recent, err := s.RefusalSummary(ctx, now.Add(-time.Hour), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range counts {
		got[c.Reason] = c.Protocol
	}
	want := map[string]string{"graphql_policy_violation": "graphql", "tool_not_pinned": "mcp", "method_not_allowed": "grpc", "ip_not_allowed": "http"}
	if len(got) != len(want) || len(recent) != 4 {
		t.Fatalf("counts %v recent %d", got, len(recent))
	}
	for r, p := range want {
		if got[r] != p {
			t.Errorf("%s classified as %q, want %q", r, got[r], p)
		}
	}
	for _, c := range counts {
		if c.Reason == "ip_not_allowed" && !c.Plugin {
			t.Error("WASM refusal not flagged")
		}
	}
	mcpOnly, _, err := s.RefusalSummary(ctx, now.Add(-time.Hour), nil, "mcp")
	if err != nil || len(mcpOnly) != 1 || mcpOnly[0].Reason != "tool_not_pinned" {
		t.Fatalf("mcp filter: %v %v", mcpOnly, err)
	}
}
