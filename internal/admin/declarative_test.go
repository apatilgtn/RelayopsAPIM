package admin

import (
	"strings"
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestParseDeclarativeConfigIsStrict(t *testing.T) {
	cases := map[string]string{
		`{"apis":[]}`:                                                     "format_version is required",
		`{"format_version":"2.0","apis":[]}`:                              "unsupported format_version",
		`{"format":"relayops-config/1.0","apis":[]}`:                      `use "format_version": "1.0"`,
		`{"format_version":"1.0","apis":[{"name":"a","path":"/a"}]}`:      `use "base_path"`,
		`{"format_version":"1.0","apis":[{"name":"a","target_url":"x"}]}`: `use "upstream_url"`,
		`{"format_version":"1.0","apis":[{"name":"a","is_active":true}]}`: `use "enabled"`,
		`{"format_version":"1.0","apis":[{"name":"a","colour":"red"}]}`:   `unknown field "colour"`,
		`{"format_version":"1.0"} {"again":1}`:                            "unexpected data",
		`{"format_version":"1.0",`:                                        "invalid JSON",
	}
	for doc, want := range cases {
		if _, err := ParseDeclarativeConfig([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", doc, err, want)
		}
	}
	cfg, err := ParseDeclarativeConfig([]byte(`{"format_version":"1.0","exported_at":"x","apis":[{"name":"a","base_path":"/a","upstream_url":"http://u"}],"plans":[]}`))
	if err != nil || len(cfg.APIs) != 1 {
		t.Fatalf("valid document rejected: %v", err)
	}
}

func current() ([]store.API, []store.Plan) {
	apis := []store.API{{
		ID: "id-orders", Name: "orders", BasePath: "/orders", UpstreamURL: "http://orders", StripPath: true,
		AuthType: "jwt", JWTSecret: "stored-secret-0123456789", TimeoutMS: 30000, Enabled: true,
		Visibility: "public", QuotaFailurePolicy: "fail_open", RequestHeaders: map[string]string{}, OpenAPISpec: map[string]any{},
	}, {
		ID: "id-legacy", Name: "legacy", BasePath: "/legacy", UpstreamURL: "http://legacy", StripPath: true,
		AuthType: "none", TimeoutMS: 30000, Enabled: true, Visibility: "public", QuotaFailurePolicy: "fail_open",
	}}
	plans := []store.Plan{{ID: "p-gold", Name: "gold", RateLimitPerMinute: 600}}
	return apis, plans
}

func mustParse(t *testing.T, doc string) DeclarativeConfig {
	t.Helper()
	cfg, err := ParseDeclarativeConfig([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func findChange(p ConfigPlan, typ, name string) *ResourceChange {
	for i := range p.Changes {
		if p.Changes[i].Type == typ && p.Changes[i].Name == name {
			return &p.Changes[i]
		}
	}
	return nil
}

func TestExportRoundTripIsANoOp(t *testing.T) {
	apis, plans := current()
	export := DeclarativeConfig{FormatVersion: "1.0"}
	for _, a := range apis {
		export.APIs = append(export.APIs, apiToDecl(a))
	}
	for _, p := range plans {
		export.Plans = append(export.Plans, planToDecl(p))
	}
	plan := ComputeConfigPlan(export, apis, plans, true, nil, nil)
	if !plan.Valid || plan.HasChanges {
		t.Fatalf("re-applying an export must change nothing (secrets are kept): %+v", plan)
	}
	if export.APIs[0].JWTSecret != "" {
		t.Fatal("export must never contain secrets")
	}
}

func TestComputeConfigPlanActions(t *testing.T) {
	apis, plans := current()
	cfg := mustParse(t, `{"format_version":"1.0",
		"plans":[{"name":"gold","rate_limit_per_minute":300},{"name":"silver","rate_limit_per_minute":60}],
		"apis":[
			{"name":"orders","base_path":"/orders","upstream_url":"http://orders-v2","auth_type":"jwt"},
			{"name":"search","base_path":"/search","upstream_url":"http://search",
			 "traffic_policy":{"targets":[{"url":"http://s1"},{"url":"http://s2"}],"retries":{"attempts":2}}}
		]}`)
	p := ComputeConfigPlan(cfg, apis, plans, false, nil, nil)
	if !p.Valid {
		t.Fatalf("errors: %v", p.Errors)
	}
	if p.Summary != (PlanSummary{Create: 2, Update: 2, Delete: 0, Unchanged: 0}) {
		t.Fatalf("summary = %+v", p.Summary)
	}
	orders := findChange(p, "api", "orders")
	if orders == nil || orders.Action != "update" || len(orders.Changes) != 1 || orders.Changes[0].Field != "upstream_url" {
		t.Fatalf("orders change = %+v (jwt secret omitted in the document must be kept, not changed)", orders)
	}
	if findChange(p, "api", "legacy") != nil {
		t.Fatal("without prune, undeclared APIs are left alone")
	}
	if gold := findChange(p, "plan", "gold"); gold.Action != "update" {
		t.Fatalf("gold = %+v", gold)
	}
	if p.Changes[0].Type != "plan" {
		t.Fatal("plans must be ordered before APIs")
	}
}

func TestComputeConfigPlanPruneWarnsAboutSubscriptions(t *testing.T) {
	apis, plans := current()
	cfg := mustParse(t, `{"format_version":"1.0","plans":[],"apis":[{"name":"orders","base_path":"/orders","upstream_url":"http://orders","auth_type":"jwt"}]}`)
	p := ComputeConfigPlan(cfg, apis, plans, true, map[string]int{"id-legacy": 3}, map[string]int{"p-gold": 2})
	legacy, gold := findChange(p, "api", "legacy"), findChange(p, "plan", "gold")
	if legacy == nil || legacy.Action != "delete" || len(legacy.Warnings) != 1 || !strings.Contains(legacy.Warnings[0], "3 subscription") {
		t.Fatalf("legacy = %+v", legacy)
	}
	if gold == nil || gold.Action != "delete" || !strings.Contains(gold.Warnings[0], "2 subscription") {
		t.Fatalf("gold = %+v", gold)
	}
}

func TestComputeConfigPlanValidationErrors(t *testing.T) {
	apis, plans := current()
	cfg := mustParse(t, `{"format_version":"1.0","plans":[{"name":"","rate_limit_per_minute":1},{"name":"x","rate_limit_per_minute":-1}],"apis":[
		{"name":"a","base_path":"no-slash","upstream_url":"http://a"},
		{"name":"b","base_path":"/b","upstream_url":"ftp://b"},
		{"name":"c","base_path":"/c","upstream_url":"http://c"},
		{"name":"c","base_path":"/c2","upstream_url":"http://c"},
		{"name":"d","base_path":"/c","upstream_url":"http://d"},
		{"name":"e","base_path":"/legacy","upstream_url":"http://e"},
		{"name":"f","base_path":"/f","upstream_url":"http://f","auth_type":"jwt","jwt_secret":"${secret:RELAYOPS_TEST_MISSING_SECRET}"},
		{"name":"g","base_path":"/g","upstream_url":"http://g","traffic_policy":{"retries":{"attempts":50}}}
	]}`)
	p := ComputeConfigPlan(cfg, apis, plans, false, nil, nil)
	if p.Valid {
		t.Fatal("plan must be invalid")
	}
	joined := strings.Join(p.Errors, "\n")
	for _, want := range []string{"name is required", "rate_limit_per_minute must be >= 0", "base_path must start", "upstream_url must be",
		"duplicate API name", `also declared by "c"`, `already used by existing API "legacy"`, "not set on the control plane", "attempts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing error %q in:\n%s", want, joined)
		}
	}
}

func TestComputeConfigPlanResolvesSecretReferences(t *testing.T) {
	t.Setenv("RELAYOPS_SECRET_BILLING_JWT", "resolved-secret-value-123456")
	cfg := mustParse(t, `{"format_version":"1.0","plans":[],"apis":[{"name":"billing","base_path":"/billing","upstream_url":"http://b","auth_type":"jwt","jwt_secret":"${secret:BILLING_JWT}"}]}`)
	p := ComputeConfigPlan(cfg, nil, nil, false, nil, nil)
	if !p.Valid || len(p.apis) != 1 || p.apis[0].JWTSecret != "resolved-secret-value-123456" {
		t.Fatalf("secret reference not resolved: valid=%v errors=%v", p.Valid, p.Errors)
	}
}

func TestPlanHashDetectsDrift(t *testing.T) {
	apis, plans := current()
	cfg := mustParse(t, `{"format_version":"1.0","plans":[],"apis":[{"name":"new","base_path":"/new","upstream_url":"http://n"}]}`)
	h1 := ComputeConfigPlan(cfg, apis, plans, false, nil, nil).PlanHash
	if h2 := ComputeConfigPlan(cfg, apis, plans, false, nil, nil).PlanHash; h1 != h2 {
		t.Fatal("plan hash must be deterministic")
	}
	apis[1].UpstreamURL = "http://someone-changed-it"
	if ComputeConfigPlan(cfg, apis, plans, false, nil, nil).PlanHash == h1 {
		t.Fatal("live-state drift must change the plan hash")
	}
	if ComputeConfigPlan(cfg, apis, plans, true, nil, nil).PlanHash == h1 {
		t.Fatal("prune flag must change the plan hash")
	}
}

func TestHS256SSOSecretFailsClosed(t *testing.T) {
	t.Setenv("RELAYOPS_SSO_SECRET", "")
	if _, err := hs256SSOSecret(); err == nil {
		t.Fatal("no secret configured must disable HS256 SSO")
	}
	t.Setenv("RELAYOPS_SSO_SECRET", "too-short")
	if _, err := hs256SSOSecret(); err == nil {
		t.Fatal("short secret must be rejected")
	}
	t.Setenv("RELAYOPS_SSO_SECRET", strings.Repeat("k", 32))
	if s, err := hs256SSOSecret(); err != nil || len(s) != 32 {
		t.Fatalf("valid secret rejected: %v", err)
	}
}

func TestValidateSplitAndAutoRollback(t *testing.T) {
	for _, sp := range []store.CanarySplit{{TrafficPercent: 101}, {TrafficPercent: -1}, {Header: "bad header"}, {HeaderValue: "x"}} {
		sp := sp
		if validateSplit(&sp) == nil {
			t.Errorf("split %+v must be rejected", sp)
		}
	}
	ok := store.CanarySplit{TrafficPercent: 10, Header: " X-Canary ", HeaderValue: "1"}
	if err := validateSplit(&ok); err != nil || ok.Header != "X-Canary" {
		t.Fatalf("valid split: %v %+v", err, ok)
	}
	base := store.AutoRollbackSettings{ErrorRateThresholdPercent: 5, EvaluationWindowSeconds: 60, MinRequests: 5, MinBaselineRequests: 20, InsufficientBaselineAction: "absolute"}
	if err := validateAutoRollback(base); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*store.AutoRollbackSettings){
		func(c *store.AutoRollbackSettings) { c.ErrorRateThresholdPercent = 0 },
		func(c *store.AutoRollbackSettings) { c.ErrorRateThresholdPercent = 150 },
		func(c *store.AutoRollbackSettings) { c.EvaluationWindowSeconds = 1 },
		func(c *store.AutoRollbackSettings) { c.MinRequests = 0 },
		func(c *store.AutoRollbackSettings) { c.CooldownSeconds = -1 },
		func(c *store.AutoRollbackSettings) { c.MinBaselineRequests = -1 },
		func(c *store.AutoRollbackSettings) { c.InsufficientBaselineAction = "ignore" },
	} {
		c := base
		mut(&c)
		if validateAutoRollback(c) == nil {
			t.Errorf("settings %+v must be rejected", c)
		}
	}
}

func TestDecideRollbackIsExplicitAboutEvidence(t *testing.T) {
	cfg := store.AutoRollbackSettings{ErrorRateThresholdPercent: 10, MinBaselineRequests: 20, InsufficientBaselineAction: "absolute"}
	res := func(candPct float64, basePct float64, baseN int) AutoRollbackEvaluationResult {
		return AutoRollbackEvaluationResult{ActiveErrorRatePct: candPct, ActiveSampleCount: 100,
			BaselineErrorRatePct: basePct, BaselineSampleCount: baseN, BaselineSufficient: baseN >= cfg.MinBaselineRequests}
	}
	cases := []struct {
		name   string
		action string
		r      AutoRollbackEvaluationResult
		breach bool
		basis  string
	}{
		{"relative breach", "absolute", res(40, 1, 500), true, "relative"},
		{"relative: baseline equally bad (shared outage)", "absolute", res(40, 39.5, 500), false, "relative"},
		{"relative: under threshold", "absolute", res(5, 0, 500), false, "relative"},
		{"thin baseline, absolute policy acts", "absolute", res(40, 0, 3), true, "absolute"},
		{"thin baseline, absolute policy healthy", "absolute", res(2, 0, 3), false, "absolute"},
		{"thin baseline, hold policy never acts", "hold", res(90, 0, 0), false, "held"},
	}
	for _, c := range cases {
		cfg.InsufficientBaselineAction = c.action
		breach, basis, reason := decideRollback(cfg, 9, 8, c.r)
		if breach != c.breach || basis != c.basis || reason == "" {
			t.Errorf("%s: breach=%v basis=%s reason=%q", c.name, breach, basis, reason)
		}
		if !c.r.BaselineSufficient && !strings.Contains(reason, "only") {
			t.Errorf("%s: reason must say the baseline is insufficient: %q", c.name, reason)
		}
	}
}

func TestDeclarativeLosslessPreservation(t *testing.T) {
	originalPlan := store.Plan{
		Name:               "enterprise-scale",
		Description:        "Full enterprise tier",
		RateLimitPerMinute: 5000,
		QuotaPerDay:        100000,
		QuotaPerMonth:      3000000,
		PriceMonthlyUSD:    299.99,
		Tier:               "enterprise",
	}

	dp := planToDecl(originalPlan)
	if dp.PriceMonthlyUSD != 299.99 || dp.Tier != "enterprise" {
		t.Fatalf("expected planToDecl to preserve price and tier, got %+v", dp)
	}

	cfg := DeclarativeConfig{
		FormatVersion: "1.0",
		Plans:         []DeclPlan{dp},
	}
	plan := computeConfigPlan(cfg, nil, []store.Plan{originalPlan}, nil, false, nil, nil)
	if len(plan.plans) != 1 {
		t.Fatalf("expected 1 plan, got %d", len(plan.plans))
	}
	if plan.plans[0].PriceMonthlyUSD != 299.99 || plan.plans[0].Tier != "enterprise" {
		t.Fatalf("expected computed plan to retain price and tier, got %+v", plan.plans[0])
	}
}

func TestDeclarativeMultiProtocolAndAIPreservation(t *testing.T) {
	originalAPI := store.API{
		Name:        "grpc-orders",
		BasePath:    "/orders.OrderService",
		UpstreamURL: "http://grpc-backend:50051",
		Protocol:    "grpc",
		GRPCPolicy: store.GRPCPolicy{
			MaxMessageSizeBytes: 10485760,
			AllowReflection:     true,
		},
		IsAI:    true,
		Enabled: true,
	}

	da := apiToDecl(originalAPI)
	if da.Protocol != "grpc" || !da.IsAI || da.GRPCPolicy == nil || !da.GRPCPolicy.AllowReflection {
		t.Fatalf("expected apiToDecl to preserve protocol and GRPCPolicy, got %+v", da)
	}

	cfg := DeclarativeConfig{
		FormatVersion: "1.0",
		APIs:          []DeclAPI{da},
	}
	plan := computeConfigPlan(cfg, []store.API{originalAPI}, nil, nil, false, nil, nil)
	if len(plan.apis) != 1 {
		t.Fatalf("expected 1 api, got %d", len(plan.apis))
	}
	res := plan.apis[0]
	if res.Protocol != "grpc" || !res.IsAI || !res.GRPCPolicy.AllowReflection || res.GRPCPolicy.MaxMessageSizeBytes != 10485760 {
		t.Fatalf("expected computed plan to preserve grpc fields and is_ai, got %+v", res)
	}
}

