package main

import (
	"slices"
	"testing"

	pt "github.com/relayops/apim/plugins/wasm/internal/plugintest"
)

func TestHeaderRewrite(t *testing.T) {
	res := pt.Run(t, handle, pt.Input{
		Headers: map[string][]string{"X-Api-Version": {"2"}, "X-Debug": {"1"}},
		Config: map[string]any{
			"set":    map[string]string{"X-Env": "production"},
			"rename": map[string]string{"x-api-version": "Accept-Version"},
			"remove": []string{"X-Debug"},
		},
	})
	pt.Expect(t, res, "modify", 0)
	if res.Headers["X-Env"] != "production" || res.Headers["Accept-Version"] != "2" {
		t.Fatalf("headers %v", res.Headers)
	}
	if !slices.Contains(res.RemoveHeaders, "X-Debug") || !slices.Contains(res.RemoveHeaders, "x-api-version") {
		t.Fatalf("removed %v", res.RemoveHeaders)
	}
}

func TestHeaderRewriteBadConfig(t *testing.T) {
	pt.Expect(t, pt.Run(t, handle, pt.Input{Config: map[string]any{"set": "not-an-object"}}), "deny", 500)
}
