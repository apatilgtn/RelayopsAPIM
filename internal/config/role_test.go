package config

import "testing"

func TestValidateRoles(t *testing.T) {
	tok := "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"default all", Config{Role: RoleAll}, true},
		{"control plane", Config{Role: RoleControlPlane, DataplaneToken: tok}, true},
		{"gateway", Config{Role: RoleGateway, ControlPlaneURL: "https://cp", DataplaneToken: tok}, true},
		{"gateway without control plane", Config{Role: RoleGateway, DataplaneToken: tok}, false},
		{"gateway without token", Config{Role: RoleGateway, ControlPlaneURL: "https://cp"}, false},
		{"short token", Config{Role: RoleControlPlane, DataplaneToken: "short"}, false},
		{"unknown role", Config{Role: "edge"}, false},
	} {
		if err := tc.cfg.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
