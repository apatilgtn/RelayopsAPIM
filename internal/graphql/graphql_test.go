package graphql

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestAnalyzeAndValidateDepth(t *testing.T) {
	query := `
		query GetUser {
			user(id: "123") {
				id
				name
				posts {
					id
					title
					comments {
						id
						text
					}
				}
			}
		}
	`
	res := Analyze(query)
	if res.OperationType != "query" {
		t.Fatalf("expected operationType query, got %s", res.OperationType)
	}
	if res.OperationName != "GetUser" {
		t.Fatalf("expected operationName GetUser, got %s", res.OperationName)
	}
	if res.Depth != 4 {
		t.Fatalf("expected depth 4, got %d", res.Depth)
	}

	policy := store.GraphQLPolicy{
		MaxDepth:       3,
		AllowMutations: true,
	}
	err := ValidatePolicy(&Request{Query: query}, res, policy)
	if err == nil || !strings.Contains(err.Error(), "exceeds configured maximum limit") {
		t.Fatalf("expected depth violation error, got: %v", err)
	}
}

func TestIntrospectionAndMutationPolicy(t *testing.T) {
	introQuery := `{ __schema { types { name } } }`
	res := Analyze(introQuery)
	if !res.IsIntrospection {
		t.Fatal("expected isIntrospection true")
	}

	policy := store.GraphQLPolicy{
		AllowIntrospection: false,
	}
	if err := ValidatePolicy(&Request{Query: introQuery}, res, policy); err == nil {
		t.Fatal("expected introspection blocked by policy")
	}

	mutationQuery := `mutation CreateItem { createItem(name: "test") { id } }`
	resMut := Analyze(mutationQuery)
	if resMut.OperationType != "mutation" {
		t.Fatalf("expected mutation, got %s", resMut.OperationType)
	}
	policyNoMut := store.GraphQLPolicy{
		AllowMutations: false,
	}
	if err := ValidatePolicy(&Request{Query: mutationQuery}, resMut, policyNoMut); err == nil {
		t.Fatal("expected mutation blocked by policy")
	}
}

func TestExtractRequest(t *testing.T) {
	// POST JSON
	body := `{"query": "query { hello }", "variables": {"x": 1}}`
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	gqlReq, err := ExtractRequest(req)
	if err != nil {
		t.Fatalf("failed to extract request: %v", err)
	}
	if gqlReq.Query != "query { hello }" {
		t.Fatalf("unexpected query: %s", gqlReq.Query)
	}
	if gqlReq.Variables["x"] != float64(1) {
		t.Fatalf("unexpected variables: %v", gqlReq.Variables)
	}
}

func TestDiffSchemas(t *testing.T) {
	baseline := `
		type User {
			id: ID!
			name: String!
			email: String
		}
		type Query {
			user(id: ID!): User
		}
	`
	// Candidate removes 'email' and changes 'name' type
	candidate := `
		type User {
			id: ID!
			name: Int!
		}
		type Query {
			user(id: ID!): User
		}
	`
	diff := DiffSchemas(baseline, candidate)
	if len(diff.BreakingChanges) != 2 {
		t.Fatalf("expected 2 breaking changes, got %d: %v", len(diff.BreakingChanges), diff.BreakingChanges)
	}
}
