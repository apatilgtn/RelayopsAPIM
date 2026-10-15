package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretValue(t *testing.T) {
	t.Setenv("RELAYOPS_SECRET_SV_ONE", `line1\nline2`)
	if v, err := SecretValue("SV_ONE"); err != nil || v != "line1\nline2" {
		t.Fatalf("escaped newlines: %q %v", v, err)
	}
	t.Setenv("SV_PLAIN", "plain")
	if v, err := SecretValue("SV_PLAIN"); err != nil || v != "plain" {
		t.Fatalf("plain name: %q %v", v, err)
	}
	p := filepath.Join(t.TempDir(), "s.pem")
	if err := os.WriteFile(p, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAYOPS_SECRET_SV_ONE_FILE", p)
	if v, err := SecretValue("SV_ONE"); err != nil || v != "from-file" {
		t.Fatalf("_FILE must win and be re-read: %q %v", v, err)
	}
	if err := os.WriteFile(p, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, _ := SecretValue("SV_ONE"); v != "rotated" {
		t.Fatalf("rotated file not re-read: %q", v)
	}
	t.Setenv("RELAYOPS_SECRET_SV_ONE_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := SecretValue("SV_ONE"); err == nil {
		t.Fatal("a missing _FILE must fail, not fall back")
	}
	if _, err := SecretValue("SV_NOT_SET_ANYWHERE"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, make([]byte, MaxSecretFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RELAYOPS_SECRET_SV_BIG_FILE", big)
	if _, err := SecretValue("SV_BIG"); err == nil {
		t.Fatal("oversized secret file accepted")
	}
}

func TestValidateCertificateSettings(t *testing.T) {
	base := func() Config { return Config{Role: RoleAll, TLSClientAuth: "optional", TLSReloadInterval: 30} }
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"cert without key", func(c *Config) { c.TLSCertFile = "a.crt" }, "set together"},
		{"crl without CA", func(c *Config) { c.TLSCertFile, c.TLSKeyFile, c.TLSClientCRLFile = "a", "b", "c.crl" }, "needs client CAs"},
		{"acme without tos", func(c *Config) { c.ACMEDomains = []string{"api.example.com"} }, "ACCEPT_TOS"},
		{"acme on control plane", func(c *Config) {
			c.Role, c.ACMEDomains, c.ACMEAcceptTOS = RoleControlPlane, []string{"api.example.com"}, true
		}, "serves no gateway listener"},
		{"short cache key", func(c *Config) { c.ACMECacheKey = "c2hvcnQ=" }, "32 random bytes"},
		{"negative reload", func(c *Config) { c.TLSReloadInterval = -1 }, "RELOAD_SECONDS"},
		{"client CA with ACME only", func(c *Config) {
			c.TLSClientCAFile, c.ACMEDomains, c.ACMEAcceptTOS = "ca.pem", []string{"api.example.com"}, true
		}, ""},
	} {
		c := base()
		tc.edit(&c)
		err := c.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}
