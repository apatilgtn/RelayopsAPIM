package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/relayops/apim/internal/graphql"
	"github.com/relayops/apim/internal/store"
)

// gqlWSServer answers each subscribe/start with one data message and
// counts the operations it received.
func gqlWSServer(t *testing.T, protocol string) (*httptest.Server, *atomic.Int64) {
	var ops atomic.Int64
	srv := httptest.NewServer(websocket.Server{
		Handshake: func(cfg *websocket.Config, r *http.Request) error {
			cfg.Protocol = []string{protocol}
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			for {
				var m map[string]any
				if err := websocket.JSON.Receive(ws, &m); err != nil {
					return
				}
				switch m["type"] {
				case "connection_init":
					_ = websocket.JSON.Send(ws, map[string]any{"type": "connection_ack"})
				case "subscribe", "start":
					ops.Add(1)
					typ := "next"
					if m["type"] == "start" {
						typ = "data"
					}
					_ = websocket.JSON.Send(ws, map[string]any{"id": m["id"], "type": typ, "payload": map[string]any{"data": map[string]any{"tick": 1}}})
				}
			}
		},
	})
	t.Cleanup(srv.Close)
	return srv, &ops
}

func gqlWSAPI(upstream string) store.API {
	api := testAPI("gqlws", "/graphql", upstream)
	api.Protocol = "graphql"
	api.GraphQLSchema = `type Query { me: String } type Subscription { tick: Int } type Mutation { reset: Boolean }`
	api.GraphQLPolicy = store.GraphQLPolicy{ValidateAgainstSchema: true, MaxDepth: 3}
	return api
}

func TestGraphQLSubscriptionsOverWebSocket(t *testing.T) {
	for _, proto := range []string{"graphql-transport-ws", "graphql-ws"} {
		t.Run(proto, func(t *testing.T) {
			up, ops := gqlWSServer(t, proto)
			g := newTestGateway(t)
			g.load(t, store.SnapshotData{APIs: []store.API{gqlWSAPI(up.URL)}})
			gw := httptest.NewServer(g)
			defer gw.Close()

			cfg, _ := websocket.NewConfig(strings.Replace(gw.URL, "http", "ws", 1)+"/graphql", gw.URL)
			cfg.Protocol = []string{proto}
			ws, err := websocket.DialConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			_ = ws.SetDeadline(time.Now().Add(5 * time.Second))
			start := "subscribe"
			if proto == "graphql-ws" {
				start = "start"
			}
			recv := func() map[string]any {
				var m map[string]any
				if err := websocket.JSON.Receive(ws, &m); err != nil {
					t.Fatalf("receive: %v", err)
				}
				return m
			}
			_ = websocket.JSON.Send(ws, map[string]any{"type": "connection_init"})
			if m := recv(); m["type"] != "connection_ack" {
				t.Fatalf("init: %v", m)
			}

			// Allowed: reaches the server, data comes back.
			_ = websocket.JSON.Send(ws, map[string]any{"id": "1", "type": start, "payload": map[string]any{"query": "subscription { tick }"}})
			if m := recv(); m["id"] != "1" || (m["type"] != "next" && m["type"] != "data") {
				t.Fatalf("allowed subscription: %v", m)
			}

			// Refused by schema validation and by the mutation policy: answered
			// by the gateway, never seen by the server.
			for id, q := range map[string]string{"2": "subscription { secrets }", "3": "mutation { reset }"} {
				_ = websocket.JSON.Send(ws, map[string]any{"id": id, "type": start, "payload": map[string]any{"query": q}})
				m := recv()
				if m["id"] != id || m["type"] != "error" {
					t.Fatalf("refused %s: %v", q, m)
				}
				raw, _ := json.Marshal(m["payload"])
				if !strings.Contains(string(raw), "message") {
					t.Fatalf("error payload %s", raw)
				}
			}
			if ops.Load() != 1 {
				t.Fatalf("server saw %d operations, want 1", ops.Load())
			}

			// The connection still works after a refusal.
			_ = websocket.JSON.Send(ws, map[string]any{"id": "4", "type": start, "payload": map[string]any{"query": "subscription { tick }"}})
			if m := recv(); m["id"] != "4" {
				t.Fatalf("after refusal: %v", m)
			}
		})
	}
}

func TestGraphQLPersistedQueries(t *testing.T) {
	up := newUpstream(t, 200)
	api := gqlWSAPI(up.srv.URL)
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	ct := map[string]string{"Content-Type": "application/json"}
	query := "{ me }"
	hash := strings.TrimPrefix(graphql.QueryHash(query), "sha256:")
	ext := `"extensions":{"persistedQuery":{"version":1,"sha256Hash":"` + hash + `"}}`

	// Unknown hash: the client is told to send the query.
	rec := do(g, "POST", "/graphql", ct, `{`+ext+`}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "PERSISTED_QUERY_NOT_FOUND") || up.hits.Load() != 0 {
		t.Fatalf("unknown hash: %d %s", rec.Code, rec.Body)
	}
	// Query + hash: verified, checked, cached and forwarded.
	if rec := do(g, "POST", "/graphql", ct, `{"query":"`+query+`",`+ext+`}`); rec.Code != 200 || up.hits.Load() != 1 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	// Hash only: the cached query is checked, the request goes upstream.
	if rec := do(g, "POST", "/graphql", ct, `{`+ext+`}`); rec.Code != 200 || up.hits.Load() != 2 {
		t.Fatalf("hash only: %d %s", rec.Code, rec.Body)
	}
	// A hash that does not match the query is refused.
	bad := `{"query":"{ me }","extensions":{"persistedQuery":{"version":1,"sha256Hash":"` + strings.Repeat("0", 64) + `"}}}`
	if rec := do(g, "POST", "/graphql", ct, bad); rec.Code != http.StatusBadRequest || up.hits.Load() != 2 {
		t.Fatalf("mismatched hash: %d %s", rec.Code, rec.Body)
	}
	// A persisted query is still subject to policy (schema validation).
	badQ := "{ secrets }"
	badExt := `"extensions":{"persistedQuery":{"version":1,"sha256Hash":"` + strings.TrimPrefix(graphql.QueryHash(badQ), "sha256:") + `"}}`
	if rec := do(g, "POST", "/graphql", ct, `{"query":"`+badQ+`",`+badExt+`}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid persisted query: %d", rec.Code)
	}
	if rec := do(g, "POST", "/graphql", ct, `{`+badExt+`}`); !strings.Contains(rec.Body.String(), "PERSISTED_QUERY_NOT_FOUND") {
		t.Fatalf("a refused query must not be cached: %s", rec.Body)
	}
}
