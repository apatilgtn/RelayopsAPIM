package main

import (
	"testing"

	pt "github.com/relayops/apim/plugins/wasm/internal/plugintest"
)

func TestIPAllowlist(t *testing.T) {
	cfg := map[string]any{"allow": []string{"10.0.0.0/8", "203.0.113.7", "2001:db8::/32"}, "deny": []string{"10.6.6.0/24"}}
	for ip, want := range map[string]string{
		"10.1.2.3":         "allow",
		"203.0.113.7":      "allow",
		"::ffff:10.1.2.3":  "allow",
		"2001:db8::1":      "allow",
		"10.6.6.9":         "deny", // deny wins
		"192.0.2.1":        "deny",
		"203.0.113.8":      "deny",
		"not-an-ip":        "deny",
	} {
		res := pt.Run(t, handle, pt.Input{ClientIP: ip, Config: cfg})
		if res.Action != want {
			t.Errorf("%s: got %+v, want %s", ip, res, want)
		}
	}
	// Deny list only: everything else is admitted.
	pt.Expect(t, pt.Run(t, handle, pt.Input{ClientIP: "192.0.2.1", Config: map[string]any{"deny": []string{"10.0.0.0/8"}}}), "allow", 0)
	pt.Expect(t, pt.Run(t, handle, pt.Input{ClientIP: "10.0.0.1", Config: map[string]any{"allow": []string{"10.0.0.0/33"}}}), "deny", 500)
}
