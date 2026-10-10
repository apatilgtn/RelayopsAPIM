// Package license loads invoice-edition entitlements from the environment.
// Caps are enforced only when RELAYOPS_EDITION is set. An unset edition is
// unlimited so existing installs and tests keep working.
package license

import (
	"os"
	"strconv"
	"strings"
)

type Edition string

const (
	EditionUnset    Edition = ""
	EditionPilot    Edition = "pilot"
	EditionTeam     Edition = "team"
	EditionBusiness Edition = "business"
)

// Entitlement is the commercial package RelayOps will invoice against.
// MaxGateways and MaxTenants of 0 mean unlimited.
type Entitlement struct {
	Edition       Edition `json:"edition"`
	MaxGateways   int     `json:"max_gateways"`
	MaxTenants    int     `json:"max_tenants"`
	PreviewAI     bool    `json:"preview_ai"`
	PreviewAPIOps bool    `json:"preview_apiops"`
	ReleaseSafety bool    `json:"release_safety"`
	Enforced      bool    `json:"enforced"`
}

// Load reads RELAYOPS_EDITION and optional overrides.
// Preview features default off. Release-safety (Studio + canary) is always on.
func Load() Entitlement {
	e := Entitlement{ReleaseSafety: true}
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("RELAYOPS_EDITION")))
	switch raw {
	case "pilot":
		e.Edition, e.MaxGateways, e.MaxTenants, e.Enforced = EditionPilot, 1, 1, true
	case "team":
		e.Edition, e.MaxGateways, e.MaxTenants, e.Enforced = EditionTeam, 3, 3, true
	case "business":
		e.Edition, e.MaxGateways, e.MaxTenants, e.Enforced = EditionBusiness, 25, 25, true
		e.PreviewAI = true
		e.PreviewAPIOps = true
	default:
		e.Edition = EditionUnset
	}
	if v := strings.TrimSpace(os.Getenv("RELAYOPS_LICENSE_MAX_GATEWAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			e.MaxGateways = n
			e.Enforced = true
		}
	}
	if v := strings.TrimSpace(os.Getenv("RELAYOPS_LICENSE_MAX_TENANTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			e.MaxTenants = n
			e.Enforced = true
		}
	}
	if v := os.Getenv("RELAYOPS_PREVIEW_AI"); v != "" {
		e.PreviewAI = envBool("RELAYOPS_PREVIEW_AI")
	}
	if v := os.Getenv("RELAYOPS_PREVIEW_APIOPS"); v != "" {
		e.PreviewAPIOps = envBool("RELAYOPS_PREVIEW_APIOPS")
	}
	return e
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// TenantLimitReached reports whether creating another tenant would exceed the edition.
func (e Entitlement) TenantLimitReached(current int) bool {
	return e.Enforced && e.MaxTenants > 0 && current >= e.MaxTenants
}

// GatewayLimitReached reports whether another gateway node would exceed the edition.
func (e Entitlement) GatewayLimitReached(current int) bool {
	return e.Enforced && e.MaxGateways > 0 && current >= e.MaxGateways
}

func (e Entitlement) Public() map[string]any {
	edition := string(e.Edition)
	if edition == "" {
		edition = "unlicensed"
	}
	return map[string]any{
		"edition":        edition,
		"max_gateways":   e.MaxGateways,
		"max_tenants":    e.MaxTenants,
		"preview_ai":     e.PreviewAI,
		"preview_apiops": e.PreviewAPIOps,
		"release_safety": true,
		"enforced":       e.Enforced,
	}
}
