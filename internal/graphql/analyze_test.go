package graphql

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func analyze(t *testing.T, q, op string, vars map[string]any) AnalysisResult {
	t.Helper()
	res, err := AnalyzeOperation(q, op, vars, nil)
	if err != nil {
		t.Fatalf("analyze %q: %v", q, err)
	}
	return res
}

// A mutation after a fragment definition used to be read as a query, which
// bypassed allow_mutations.
func TestMutationAfterFragmentIsAMutation(t *testing.T) {
	q := `fragment F on User { id } mutation Drop { deleteAll { ...F } }`
	res := analyze(t, q, "", nil)
	if res.OperationType != "mutation" || res.OperationName != "Drop" {
		t.Fatalf("got %s %s", res.OperationType, res.OperationName)
	}
	if err := ValidatePolicy(&Request{Query: q}, res, store.GraphQLPolicy{}); err == nil {
		t.Fatal("mutation allowed with allow_mutations=false")
	}
	// With several operations, operationName selects the one that runs.
	multi := `query Read { me { id } } mutation Write { deleteAll { id } }`
	if _, err := AnalyzeOperation(multi, "", nil, nil); err == nil {
		t.Fatal("ambiguous document accepted without operationName")
	}
	if res := analyze(t, multi, "Write", nil); res.OperationType != "mutation" {
		t.Fatalf("operationName Write analysed as %s", res.OperationType)
	}
	if _, err := AnalyzeOperation(multi, "Missing", nil, nil); err == nil {
		t.Fatal("unknown operationName accepted")
	}
}

// Fragments used to hide nesting from the brace counter.
func TestDepthFollowsFragments(t *testing.T) {
	q := `query { a { ...L1 } } fragment L1 on T { b { ...L2 } } fragment L2 on T { c { d { e } } }`
	if res := analyze(t, q, "", nil); res.Depth != 5 {
		t.Fatalf("depth %d, want 5", res.Depth)
	}
	// Braces in arguments are not selection depth.
	if res := analyze(t, `{ search(filter: {a: {b: {c: 1}}}) { id } }`, "", nil); res.Depth != 2 {
		t.Fatalf("argument objects counted as depth: %d", res.Depth)
	}
}

func TestFragmentCycleRejected(t *testing.T) {
	if _, err := AnalyzeOperation(`{ a { ...A } } fragment A on T { b { ...B } } fragment B on T { c { ...A } }`, "", nil, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}
}

// Each fragment spreads the next one twice: 2^30 fields if expanded naively.
func TestFragmentFanOutIsMeasuredInLinearTime(t *testing.T) {
	var b strings.Builder
	b.WriteString("{ root { ...F0 } }\n")
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "fragment F%d on T { a: x { ...F%d } b: x { ...F%d } }\n", i, i+1, i+1)
	}
	b.WriteString("fragment F30 on T { leaf }\n")
	start := time.Now()
	res := analyze(t, b.String(), "", nil)
	if time.Since(start) > time.Second {
		t.Fatalf("analysis took %v", time.Since(start))
	}
	if res.Cost < 1<<30 || res.Depth != 32 {
		t.Fatalf("cost %d depth %d", res.Cost, res.Depth)
	}
	if err := ValidatePolicy(&Request{}, res, store.GraphQLPolicy{MaxCost: 10000}); err == nil {
		t.Fatal("exponential query under the cost limit")
	}
}

func TestIntrospectionFromFieldsNotText(t *testing.T) {
	if res := analyze(t, `{ search(q: "__schema") { id } }`, "", nil); res.IsIntrospection {
		t.Fatal("a string argument flagged as introspection")
	}
	if res := analyze(t, `{ s: __schema { types { name } } }`, "", nil); !res.IsIntrospection {
		t.Fatal("aliased __schema not detected")
	}
	if res := analyze(t, `{ me { __typename } }`, "", nil); res.IsIntrospection {
		t.Fatal("__typename is not schema introspection")
	}
}

func TestCostMultipliesListSizes(t *testing.T) {
	// users(first: 100) { friends(first: 10) { name } }: 1 + 100 * (1 + 10 * 1)
	q := `query($n: Int) { users(first: 100) { friends(first: $n) { name } } }`
	if res := analyze(t, q, "", map[string]any{"n": 10}); res.Cost != 1+100*(1+10) {
		t.Fatalf("cost %d", res.Cost)
	}
	if res := analyze(t, `{ users { id name } }`, "", nil); res.Cost != 3 {
		t.Fatalf("unpaged cost %d", res.Cost)
	}
}

func TestAliasesAndAllowlistByHash(t *testing.T) {
	q := `query Feed { a: me { id } b: me { id } c: me { id } }`
	res := analyze(t, q, "", nil)
	if res.Aliases != 3 {
		t.Fatalf("aliases %d", res.Aliases)
	}
	if err := ValidatePolicy(&Request{}, res, store.GraphQLPolicy{MaxAliases: 2}); err == nil {
		t.Fatal("alias limit not applied")
	}
	byHash := store.GraphQLPolicy{OperationAllowlist: []string{QueryHash(q)}}
	if err := ValidatePolicy(&Request{}, res, byHash); err != nil {
		t.Fatalf("exact persisted query refused: %v", err)
	}
	renamed := analyze(t, `query Feed { everything { secret } }`, "", nil)
	if err := ValidatePolicy(&Request{}, renamed, byHash); err == nil {
		t.Fatal("a different document named Feed passed the hash allowlist")
	}
}

func TestValidateAgainstSchema(t *testing.T) {
	schema, err := LoadSchema(`type Query { user(id: ID!): User } type User { id: ID! name: String }`)
	if err != nil {
		t.Fatal(err)
	}
	if errs := ValidateAgainstSchema(schema, `{ user(id: "1") { id name } }`); len(errs) != 0 {
		t.Fatalf("valid query: %v", errs)
	}
	errs := ValidateAgainstSchema(schema, `{ user(id: "1") { id password } }`)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "password") {
		t.Fatalf("unknown field: %v", errs)
	}
	if errs := ValidateAgainstSchema(schema, `{ user { id } }`); len(errs) == 0 {
		t.Fatal("missing required argument accepted")
	}
	if errs := ValidateAgainstSchema(schema, `{ __schema { types { name } } }`); len(errs) != 0 {
		t.Fatalf("introspection should validate (policy decides): %v", errs)
	}
	if _, err := LoadSchema(`type Query { broken: Nope }`); err == nil {
		t.Fatal("invalid SDL loaded")
	}
}
