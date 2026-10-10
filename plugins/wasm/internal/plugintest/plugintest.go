// Package plugintest runs plugin handlers natively through the same JSON
// boundary the gateway uses.
package plugintest

import (
	"encoding/json"
	"testing"

	plugin "github.com/relayops/apim/sdk/wasmplugin"
)

// Input is a request as the gateway encodes it.
type Input struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	ClientIP string              `json:"client_ip"`
	Headers  map[string][]string `json:"headers,omitempty"`
	Query    map[string][]string `json:"query_params,omitempty"`
	Body     *string             `json:"body,omitempty"`
	Config   any                 `json:"config,omitempty"`
}

// Body returns a pointer for Input.Body.
func Body(s string) *string { return &s }

// Run dispatches in to h and decodes the result.
func Run(t *testing.T, h plugin.Handler, in Input) plugin.Result {
	t.Helper()
	if in.Method == "" {
		in.Method = "GET"
	}
	if in.Path == "" {
		in.Path = "/"
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var res plugin.Result
	if err := json.Unmarshal(plugin.Dispatch(h, raw), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// Expect fails unless res has the action (and, for a deny, the status).
func Expect(t *testing.T, res plugin.Result, action string, status int) {
	t.Helper()
	if res.Action != action || (action == "deny" && res.Status != status) {
		t.Fatalf("got %+v, want %s %d", res, action, status)
	}
}
