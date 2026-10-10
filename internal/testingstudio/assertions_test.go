package testingstudio

import (
	"net/http"
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestEvaluateAssertions(t *testing.T) {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("X-Custom-Trace", "trace-12345")
	headers.Set("X-RelayOps-Revision", "rev_0000000005")

	body := []byte(`{
		"status": "success",
		"data": {
			"id": "item-999",
			"count": 42,
			"items": ["apple", "banana"]
		},
		"timestamp": 1700000000
	}`)

	statusCode := 200
	durationMS := 15.0

	tests := []struct {
		name     string
		def      store.AssertionDef
		expected bool
	}{
		{
			name:     "status_code matching",
			def:      store.AssertionDef{Type: "status_code", Expected: "200"},
			expected: true,
		},
		{
			name:     "status_code mismatch",
			def:      store.AssertionDef{Type: "status_code", Expected: "404"},
			expected: false,
		},
		{
			name:     "header_equals exact match",
			def:      store.AssertionDef{Type: "header_equals", Target: "X-Custom-Trace", Expected: "trace-12345"},
			expected: true,
		},
		{
			name:     "header_exists true",
			def:      store.AssertionDef{Type: "header_exists", Target: "X-RelayOps-Revision"},
			expected: true,
		},
		{
			name:     "header_exists false",
			def:      store.AssertionDef{Type: "header_exists", Target: "Non-Existent-Header"},
			expected: false,
		},
		{
			name:     "json_path_equals string value",
			def:      store.AssertionDef{Type: "json_path_equals", Target: "/data/id", Expected: "item-999"},
			expected: true,
		},
		{
			name:     "json_path_equals integer value",
			def:      store.AssertionDef{Type: "json_path_equals", Target: "/data/count", Expected: "42"},
			expected: true,
		},
		{
			name:     "json_path_exists array element",
			def:      store.AssertionDef{Type: "json_path_exists", Target: "/data/items/0"},
			expected: true,
		},
		{
			name:     "json_path_exists non-existent",
			def:      store.AssertionDef{Type: "json_path_exists", Target: "/data/missing"},
			expected: false,
		},
		{
			name:     "body_contains substring",
			def:      store.AssertionDef{Type: "body_contains", Expected: "banana"},
			expected: true,
		},
		{
			name:     "response_time_ms below threshold",
			def:      store.AssertionDef{Type: "response_time_ms", Expected: "50"},
			expected: true,
		},
		{
			name:     "response_time_ms exceeded threshold",
			def:      store.AssertionDef{Type: "response_time_ms", Expected: "5"},
			expected: false,
		},
		{
			name:     "graphql_has_no_errors true",
			def:      store.AssertionDef{Type: "graphql_has_no_errors"},
			expected: true,
		},
		{
			name:     "graphql_data_equals match",
			def:      store.AssertionDef{Type: "graphql_data_equals", Target: "id", Expected: "item-999"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := EvaluateAssertion(tt.def, statusCode, headers, body, durationMS)
			if res.Passed != tt.expected {
				t.Errorf("Assertion %s: expected passed=%v, got %v (error: %s, actual: %s)",
					tt.name, tt.expected, res.Passed, res.Error, res.Actual)
			}
		})
	}
}
