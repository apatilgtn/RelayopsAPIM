package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionPreflight(t *testing.T) {
	t.Run("development compatibility", func(t *testing.T) {
		if err := (Config{}).ValidateProduction(); err != nil {
			t.Fatal(err)
		}
	})
	cp := Config{Environment: "production", Role: RoleControlPlane, AdminToken: strings.Repeat("x", 40), AdminTLSCertFile: "cert", AdminTLSKeyFile: "key", DataplaneAPI: true, DataplaneSigningKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	gw := Config{Environment: "production", Role: RoleGateway, ControlPlaneURL: "https://control.example", DataplaneToken: "rlnode_unique", DataplaneVerifyKeys: base64.StdEncoding.EncodeToString(make([]byte, 32)), DataDir: t.TempDir(), PersistentData: true, LogSpoolMB: 10}
	for name, c := range map[string]Config{"control plane": cp, "gateway": gw} {
		if err := c.ValidateProduction(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, change := range []func(*Config){func(c *Config) { c.AdminToken = "relayops-admin" }, func(c *Config) { c.AdminTLSCertFile = "" }, func(c *Config) { c.DataplaneSigningKey = "" }, func(c *Config) { c.DataplaneToken = "shared-secret" }} {
		c := cp
		change(&c)
		if c.ValidateProduction() == nil {
			t.Fatal("insecure control plane accepted")
		}
	}
	for _, change := range []func(*Config){func(c *Config) { c.ControlPlaneURL = "http://control.example" }, func(c *Config) { c.DataplaneAllowHTTP = true }, func(c *Config) { c.DataplaneToken = "shared" }, func(c *Config) { c.DataplaneVerifyKeys = "" }, func(c *Config) { c.PersistentData = false }, func(c *Config) { c.DataDir = "relative" }, func(c *Config) { c.LogSpoolMB = 0 }} {
		c := gw
		change(&c)
		if c.ValidateProduction() == nil {
			t.Fatal("insecure gateway accepted")
		}
	}
	if err := gw.CheckRecoveryStorage(); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	gw.DataDir = blocked
	if err := gw.CheckRecoveryStorage(); err == nil {
		t.Fatal("unavailable storage accepted")
	}
	if err := (Config{Environment: "prod"}).ValidateProduction(); err == nil {
		t.Fatal("unknown environment accepted")
	}
	gw.Role = RoleRelay
	if gw.ValidateProduction() == nil {
		t.Fatal("relay without persistent signed recovery accepted in production")
	}
}
