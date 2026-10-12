package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ProductionChecks contains names and remediation only; never configuration values.
func (c Config) ProductionChecks() []string {
	issues := []string{}
	if c.Environment != "" && c.Environment != "development" && c.Environment != "production" {
		return []string{"RELAYOPS_ENV must be development or production"}
	}
	if c.Environment != "production" {
		return issues
	}
	if c.Role == RoleAll {
		issues = append(issues, "production requires separated control-plane and gateway roles")
	}
	if c.Role == RoleRelay {
		issues = append(issues, "production relay recovery is not supported; connect gateways directly to the redundant control plane")
	}
	if c.Role == RoleControlPlane {
		placeholder := strings.ToLower(c.AdminToken)
		if len(c.AdminToken) < 32 || c.AdminToken == "relayops-admin" || strings.Contains(placeholder, "replace") || strings.Contains(placeholder, "changeme") {
			issues = append(issues, "set a unique RELAYOPS_ADMIN_TOKEN of at least 32 characters")
		}
		if c.AdminTLSCertFile == "" || c.AdminTLSKeyFile == "" {
			issues = append(issues, "configure admin TLS certificate and key")
		}
		if c.DataplaneToken != "" {
			issues = append(issues, "remove the shared control-plane dataplane token; issue per-node credentials")
		}
		if !c.DataplaneAPI || c.DataplaneSigningKey == "" {
			issues = append(issues, "enable the node API and configure its Ed25519 signing key")
		}
		if c.DataplaneSigningKey != "" {
			raw, err := base64.StdEncoding.DecodeString(c.DataplaneSigningKey)
			if err != nil || (len(raw) != 32 && len(raw) != 64) {
				issues = append(issues, "use a base64 Ed25519 signing seed or private key")
			}
		}
	}
	if c.Role == RoleGateway || c.Role == RoleRelay {
		u, err := url.Parse(c.ControlPlaneURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || c.DataplaneAllowHTTP {
			issues = append(issues, "use an HTTPS control-plane URL and disable dataplane allow-HTTP")
		}
		if c.DataplaneVerifyKeys == "" {
			issues = append(issues, "configure trusted configuration verification keys")
		}
		for _, key := range strings.Split(c.DataplaneVerifyKeys, ",") {
			if key == "" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
			if err != nil || len(raw) != 32 {
				issues = append(issues, "use base64 Ed25519 public verification keys")
				break
			}
		}
		if !strings.HasPrefix(c.DataplaneToken, "rlnode_") {
			issues = append(issues, "supply a per-node dataplane credential issued by the control plane")
		}
		if !filepath.IsAbs(c.DataDir) || !c.PersistentData {
			issues = append(issues, "set an absolute RELAYOPS_DATA_DIR on durable storage and RELAYOPS_PERSISTENT_DATA=true")
		}
		if c.LogSpoolMB <= 0 {
			issues = append(issues, "enable the durable request-log spool")
		}
	}
	return issues
}

// CheckRecoveryStorage proves writability, not the durability of the underlying volume.
// Deployment operators must mount durable storage and explicitly attest that fact.
func (c Config) CheckRecoveryStorage() error {
	if c.Environment != "production" || (c.Role != RoleGateway && c.Role != RoleRelay) {
		return nil
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return errors.New("recovery storage directory is unavailable")
	}
	f, err := os.CreateTemp(c.DataDir, ".relayops-preflight-")
	if err != nil {
		return errors.New("recovery storage is not writable")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.WriteString("storage check"); err != nil {
		f.Close()
		return errors.New("recovery storage write failed")
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return errors.New("recovery storage sync failed")
	}
	return f.Close()
}

func DataDirectory() string { return env("RELAYOPS_DATA_DIR", "data") }
func (c Config) ValidateProduction() error {
	if issues := c.ProductionChecks(); len(issues) > 0 {
		return fmt.Errorf("configuration preflight: %s", strings.Join(issues, "; "))
	}
	return nil
}
