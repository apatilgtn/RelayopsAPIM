package gateway

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestGraphQLStructureAwarePolicy(t *testing.T) {
	up := newUpstream(t, 200)
	api := testAPI("gql", "/graphql", up.srv.URL)
	api.Protocol = "graphql"
	api.GraphQLSchema = `type Query { user(id: ID!): User users(first: Int): [User] }
		type Mutation { deleteUser(id: ID!): Boolean }
		type User { id: ID! name: String friends(first: Int): [User] }`
	api.GraphQLPolicy = store.GraphQLPolicy{MaxDepth: 4, MaxCost: 500, MaxAliases: 5, MaxBatchSize: 2,
		ValidateAgainstSchema: true, AllowMutations: false}
	g := newTestGateway(t)
	g.load(t, store.SnapshotData{APIs: []store.API{api}})
	json := map[string]string{"Content-Type": "application/json"}

	cases := []struct {
		name, body string
		code       int
		reason     string
	}{
		{"valid", `{"query":"{ user(id: \"1\") { id name } }"}`, 200, ""},
		{"unknown field", `{"query":"{ user(id: \"1\") { id ssn } }"}`, 400, "graphql_validation_failed"},
		{"mutation after fragment", `{"query":"fragment F on User { id } mutation { deleteUser(id: \"1\") }"}`, 400, "graphql_policy_violation"},
		{"depth via fragments", `{"query":"{ user(id: \"1\") { ...A } } fragment A on User { friends { friends { friends { id } } } }"}`, 400, "graphql_policy_violation"},
		{"list cost", `{"query":"{ users(first: 100) { friends(first: 100) { id } } }"}`, 400, "graphql_policy_violation"},
		{"syntax error", `{"query":"{ user(id: \"1\") { id "}`, 400, "invalid_graphql_request"},
		{"batch within limit", `[{"query":"{ user(id: \"1\") { id } }"},{"query":"{ user(id: \"2\") { id } }"}]`, 200, ""},
		{"batch over limit", `[{"query":"{ a: user(id: \"1\") { id } }"},{"query":"{ user(id: \"2\") { id } }"},{"query":"{ user(id: \"3\") { id } }"}]`, 400, "invalid_graphql_request"},
		{"bad operation in batch", `[{"query":"{ user(id: \"1\") { id } }"},{"query":"{ user(id: \"2\") { nope } }"}]`, 400, "graphql_validation_failed"},
	}
	for _, c := range cases {
		before := up.hits.Load()
		rec := do(g, "POST", "/graphql", json, c.body)
		if rec.Code != c.code {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
			continue
		}
		if c.code != 200 {
			if up.hits.Load() != before {
				t.Errorf("%s: refused request reached the upstream", c.name)
			}
			if got := rec.Header().Get("X-RelayOps-Decision-Reason"); got != c.reason {
				t.Errorf("%s: reason %q, want %q", c.name, got, c.reason)
			}
			if !strings.Contains(rec.Body.String(), `"errors"`) {
				t.Errorf("%s: not a GraphQL error body: %s", c.name, rec.Body)
			}
		}
	}

	// Mutations over GET are refused (CSRF), queries over GET work.
	q := url.QueryEscape(`mutation { deleteUser(id: "1") }`)
	if rec := do(g, "GET", "/graphql?query="+q, nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("mutation over GET: %d", rec.Code)
	}
	if rec := do(g, "GET", "/graphql?query="+url.QueryEscape(`{ user(id: "1") { id } }`), nil, ""); rec.Code != 200 {
		t.Fatalf("query over GET: %d %s", rec.Code, rec.Body)
	}

	// An invalid schema fails the API rather than skipping validation.
	broken := api
	broken.GraphQLSchema = "type Query { x: Missing }"
	if _, errs := buildSnapshot(store.SnapshotData{APIs: []store.API{broken}}, 1, newUpstreamRegistry()); len(errs) == 0 {
		t.Fatal("invalid schema accepted")
	}
}
