package main

import (
	"testing"

	pt "github.com/relayops/apim/plugins/wasm/internal/plugintest"
)

func TestRequestValidator(t *testing.T) {
	cfg := map[string]any{
		"methods": []string{"GET", "POST"}, "required_headers": []string{"X-Tenant"},
		"content_types": []string{"application/json"}, "json_required": []string{"order.id", "amount"}, "max_body_bytes": 200,
	}
	h := map[string][]string{"X-Tenant": {"acme"}, "Content-Type": {"application/json; charset=utf-8"}}
	ok := `{"order":{"id":"o-1"},"amount":3}`
	cases := []struct {
		name   string
		in     pt.Input
		action string
		status int
	}{
		{"valid", pt.Input{Method: "POST", Headers: h, Body: pt.Body(ok)}, "allow", 0},
		{"get skips body checks", pt.Input{Method: "GET", Headers: h}, "allow", 0},
		{"method", pt.Input{Method: "DELETE", Headers: h}, "deny", 405},
		{"header", pt.Input{Method: "GET"}, "deny", 400},
		{"content type", pt.Input{Method: "POST", Headers: map[string][]string{"X-Tenant": {"a"}, "Content-Type": {"text/plain"}}, Body: pt.Body(ok)}, "deny", 415},
		{"invalid json", pt.Input{Method: "POST", Headers: h, Body: pt.Body("{")}, "deny", 400},
		{"missing nested", pt.Input{Method: "POST", Headers: h, Body: pt.Body(`{"order":{},"amount":1}`)}, "deny", 400},
		{"null field", pt.Input{Method: "POST", Headers: h, Body: pt.Body(`{"order":{"id":"x"},"amount":null}`)}, "deny", 400},
		{"too large", pt.Input{Method: "POST", Headers: h, Body: pt.Body(`{"order":{"id":"` + string(make([]byte, 300)) + `"},"amount":1}`)}, "deny", 413},
		{"no body access", pt.Input{Method: "POST", Headers: h}, "deny", 500},
	}
	for _, c := range cases {
		c.in.Config = cfg
		res := pt.Run(t, handle, c.in)
		if res.Action != c.action || (c.action == "deny" && res.Status != c.status) {
			t.Errorf("%s: got %+v, want %s %d", c.name, res, c.action, c.status)
		}
	}
}
