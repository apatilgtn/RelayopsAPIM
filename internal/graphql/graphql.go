package graphql

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/relayops/apim/internal/store"
)

// Request represents an incoming GraphQL query/mutation request.
type Request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
	Extensions    Extensions     `json:"extensions,omitempty"`
}

// hasPersisted reports whether the request names a persisted query.
func (r Request) hasPersisted() bool {
	return r.Extensions.PersistedQuery != nil && r.Extensions.PersistedQuery.Sha256Hash != ""
}

// AnalysisResult contains static analysis metrics for a GraphQL query.
type AnalysisResult struct {
	OperationType   string   // "query", "mutation", "subscription", or "unknown"
	OperationName   string   // parsed operation name
	Depth           int      // maximum selection set nesting depth
	Cost            int      // estimated field complexity cost
	IsIntrospection bool     // true if the operation selects __schema or __type
	TopLevelFields  []string // top-level requested fields
	Aliases         int      // fields renamed with an alias (incl. via fragments)
	Hash            string   // sha256 of the query text (persisted-query id)
}

// ExtractRequest parses a single GraphQL request from an HTTP POST or GET.
func ExtractRequest(r *http.Request) (*Request, error) {
	reqs, _, err := ExtractRequests(r, 0)
	if err != nil {
		return nil, err
	}
	return &reqs[0], nil
}

// ExtractRequests parses a GraphQL request, or a batch (a JSON array) when
// maxBatch > 0. The body is restored for the upstream.
func ExtractRequests(r *http.Request, maxBatch int) ([]Request, bool, error) {
	if r.Method == http.MethodGet {
		q := r.URL.Query().Get("query")
		req := Request{Query: q, OperationName: r.URL.Query().Get("operationName")}
		if vStr := r.URL.Query().Get("variables"); vStr != "" {
			if err := json.Unmarshal([]byte(vStr), &req.Variables); err != nil {
				return nil, false, fmt.Errorf("invalid 'variables' parameter: %w", err)
			}
		}
		if eStr := r.URL.Query().Get("extensions"); eStr != "" {
			if err := json.Unmarshal([]byte(eStr), &req.Extensions); err != nil {
				return nil, false, fmt.Errorf("invalid 'extensions' parameter: %w", err)
			}
		}
		if strings.TrimSpace(q) == "" && !req.hasPersisted() {
			return nil, false, errors.New("missing GraphQL 'query' parameter")
		}
		return []Request{req}, false, nil
	}
	if r.Method != http.MethodPost {
		return nil, false, fmt.Errorf("unsupported HTTP method for GraphQL: %s", r.Method)
	}
	if r.Body == nil {
		return nil, false, errors.New("empty request body")
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, false, fmt.Errorf("read body: %w", err)
	}
	if len(bodyBytes) > maxBody {
		return nil, false, fmt.Errorf("GraphQL request body exceeds %d bytes", maxBody)
	}
	// Restore body for downstream proxying
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/graphql") {
		return []Request{{Query: string(bodyBytes)}}, false, nil
	}
	if trimmed := bytes.TrimSpace(bodyBytes); len(trimmed) > 0 && trimmed[0] == '[' {
		if maxBatch <= 0 {
			return nil, true, errors.New("batched GraphQL requests are not allowed on this API")
		}
		var reqs []Request
		if err := json.Unmarshal(trimmed, &reqs); err != nil {
			return nil, true, fmt.Errorf("invalid GraphQL batch: %w", err)
		}
		if len(reqs) == 0 || len(reqs) > maxBatch {
			return nil, true, fmt.Errorf("GraphQL batch of %d operations; this API allows 1 to %d", len(reqs), maxBatch)
		}
		for i := range reqs {
			if strings.TrimSpace(reqs[i].Query) == "" && !reqs[i].hasPersisted() {
				return nil, true, fmt.Errorf("GraphQL batch entry %d has no query", i)
			}
		}
		return reqs, true, nil
	}
	var req Request
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, false, fmt.Errorf("invalid GraphQL JSON body: %w", err)
	}
	if strings.TrimSpace(req.Query) == "" && !req.hasPersisted() {
		return nil, false, errors.New("GraphQL query cannot be empty")
	}
	return []Request{req}, false, nil
}

const maxBody = 2 << 20

// Analyze measures a document's only (or first-named) operation. A document
// that cannot be parsed reports operation type "invalid".
func Analyze(query string) AnalysisResult {
	res, err := AnalyzeOperation(query, "", nil, nil)
	if err != nil {
		res.OperationType = "invalid"
	}
	return res
}

// ValidatePolicy checks if the analyzed request adheres to the API's GraphQL policy.
func ValidatePolicy(req *Request, analysis AnalysisResult, policy store.GraphQLPolicy) error {
	if analysis.IsIntrospection && !policy.AllowIntrospection {
		return errors.New("GraphQL introspection queries are disabled by policy")
	}

	if analysis.OperationType == "mutation" && !policy.AllowMutations {
		return errors.New("GraphQL mutations are disabled on this API endpoint")
	}

	if policy.MaxDepth > 0 && analysis.Depth > policy.MaxDepth {
		return fmt.Errorf("GraphQL query depth (%d) exceeds configured maximum limit (%d)", analysis.Depth, policy.MaxDepth)
	}

	if policy.MaxCost > 0 && analysis.Cost > policy.MaxCost {
		return fmt.Errorf("GraphQL query complexity cost (%d) exceeds configured maximum limit (%d)", analysis.Cost, policy.MaxCost)
	}

	if policy.MaxAliases > 0 && analysis.Aliases > policy.MaxAliases {
		return fmt.Errorf("GraphQL query uses %d aliases; the limit is %d", analysis.Aliases, policy.MaxAliases)
	}

	// Allowlist entries are operation names, or "sha256:<hex>" hashes of the
	// full query text. A hash pins the exact document; a name only says what
	// the client called its operation.
	if len(policy.OperationAllowlist) > 0 {
		allowed := false
		for _, op := range policy.OperationAllowlist {
			if op == analysis.Hash || (!strings.HasPrefix(op, "sha256:") && op != "" && op == analysis.OperationName) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("operation %q (%s) is not in the configured operation allowlist", analysis.OperationName, analysis.Hash)
		}
	}

	return nil
}

// FormatGraphQLError returns a standard GraphQL error JSON response payload.
func FormatGraphQLError(message string) map[string]any {
	return map[string]any{
		"errors": []map[string]any{
			{
				"message": message,
				"extensions": map[string]any{
					"code": "RELAYOPS_POLICY_VIOLATION",
				},
			},
		},
	}
}
