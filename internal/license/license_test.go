package license

import (
	"testing"
)

func TestLoadDefaultsUnlimitedAndPreviewOff(t *testing.T) {
	t.Setenv("RELAYOPS_EDITION", "")
	t.Setenv("RELAYOPS_LICENSE_MAX_GATEWAYS", "")
	t.Setenv("RELAYOPS_LICENSE_MAX_TENANTS", "")
	t.Setenv("RELAYOPS_PREVIEW_AI", "")
	t.Setenv("RELAYOPS_PREVIEW_APIOPS", "")
	e := Load()
	if e.Enforced || e.PreviewAI || e.PreviewAPIOps || !e.ReleaseSafety {
		t.Fatalf("unset edition must be unlimited with preview off: %+v", e)
	}
	if e.TenantLimitReached(100) || e.GatewayLimitReached(100) {
		t.Fatal("unset edition must not cap tenants or gateways")
	}
}

func TestLoadTeamCaps(t *testing.T) {
	t.Setenv("RELAYOPS_EDITION", "team")
	e := Load()
	if e.Edition != EditionTeam || e.MaxTenants != 3 || e.MaxGateways != 3 || !e.Enforced {
		t.Fatalf("team: %+v", e)
	}
	if !e.TenantLimitReached(3) || e.TenantLimitReached(2) {
		t.Fatalf("team tenant cap: reached(3)=%v reached(2)=%v", e.TenantLimitReached(3), e.TenantLimitReached(2))
	}
}

func TestLoadOverridesAndPreview(t *testing.T) {
	t.Setenv("RELAYOPS_EDITION", "pilot")
	t.Setenv("RELAYOPS_LICENSE_MAX_TENANTS", "5")
	t.Setenv("RELAYOPS_PREVIEW_AI", "true")
	e := Load()
	if e.Edition != EditionPilot || e.MaxTenants != 5 || e.MaxGateways != 1 || !e.PreviewAI || e.PreviewAPIOps {
		t.Fatalf("overrides: %+v", e)
	}
}

func TestLoadBusinessDefaultsWithPreview(t *testing.T) {
	t.Setenv("RELAYOPS_EDITION", "business")
	e := Load()
	if e.Edition != EditionBusiness || e.MaxTenants != 25 || e.MaxGateways != 25 || !e.PreviewAI || !e.PreviewAPIOps {
		t.Fatalf("business edition must enable preview AI and APIOps: %+v", e)
	}
}
