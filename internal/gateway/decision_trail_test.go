package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/analytics"
	"github.com/relayops/apim/internal/realtime"
	"github.com/relayops/apim/internal/store"
)

// Protocol inspection runs before authentication; its findings must stay in
// the logged decision trail next to the authentication result.
func TestDecisionTrailKeepsProtocolInspection(t *testing.T) {
	hub := realtime.NewHub()
	sub := hub.Subscribe()
	c := analytics.NewCollector(nil, hub, "trail")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	g := New(c, nil, "trail")

	f := newFakeMCPServer(t)
	api := testAPI("trail", "/trail", f.srv.URL)
	api.Protocol = "mcp"
	api.MCPPolicy = store.MCPPolicy{Rules: []store.MCPToolRule{{Tools: []string{"shell"}, Action: "deny"}}}
	api.MCPPolicy.Normalize()
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	rpc(g, "/trail", nil, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell"}}`)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-sub:
			if msg.Event != "tick" {
				continue
			}
			var tick analytics.Tick
			_ = json.Unmarshal(msg.Data, &tick)
			for _, l := range tick.Recent {
				if !strings.HasPrefix(l.Path, "/trail") {
					continue
				}
				mcp, _ := l.PolicyEvaluations["mcp"].(map[string]any)
				if mcp == nil || mcp["refused"] != "tool_not_allowed" {
					t.Fatalf("MCP findings missing from the trail: %v", l.PolicyEvaluations)
				}
				if _, ok := l.PolicyEvaluations["routing"]; !ok {
					t.Fatalf("routing missing from the trail: %v", l.PolicyEvaluations)
				}
				return
			}
		case <-deadline:
			t.Fatal("request never logged")
		}
	}
}
