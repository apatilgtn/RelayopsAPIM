package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/apiops"
	"github.com/relayops/apim/internal/config"
	"github.com/relayops/apim/internal/store"
)

// DeclarativeFormatVersion is the only accepted format_version.
const DeclarativeFormatVersion = apiops.DeclarativeFormatVersion

type DeclarativeConfig = apiops.DeclarativeConfig
type DeclAPI = apiops.DeclAPI
type DeclPlan = apiops.DeclPlan


// fieldHints maps common mistaken field names to the correct ones.
var fieldHints = map[string]string{
	"format":     `use "format_version": "1.0"`,
	"path":       `use "base_path"`,
	"target_url": `use "upstream_url"`,
	"upstream":   `use "upstream_url"`,
	"is_active":  `use "enabled"`,
	"active":     `use "enabled"`,
	"rate_limit": `use "rate_limit_per_minute"`,
	"timeout":    `use "timeout_ms"`,
	"auth":       `use "auth_type"`,
	"id":         `APIs and plans are matched by "name"; remove "id"`,
	"created_at": `remove server-managed field "created_at"`,
	"updated_at": `remove server-managed field "updated_at"`,
}

// ParseDeclarativeConfig strictly decodes a declarative document.
func ParseDeclarativeConfig(raw []byte) (DeclarativeConfig, error) {
	var cfg DeclarativeConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		msg := err.Error()
		if i := strings.Index(msg, `unknown field "`); i >= 0 {
			name := strings.TrimSuffix(msg[i+len(`unknown field "`):], `"`)
			if hint, ok := fieldHints[name]; ok {
				return cfg, fmt.Errorf("unknown field %q: %s", name, hint)
			}
			return cfg, fmt.Errorf("unknown field %q", name)
		}
		return cfg, fmt.Errorf("invalid JSON: %s", msg)
	}
	if dec.More() {
		return cfg, errors.New("invalid JSON: unexpected data after the document")
	}
	if cfg.FormatVersion == "" {
		return cfg, fmt.Errorf(`format_version is required (use "format_version": %q)`, DeclarativeFormatVersion)
	}
	if cfg.FormatVersion != DeclarativeFormatVersion {
		return cfg, fmt.Errorf("unsupported format_version %q (supported: %q)", cfg.FormatVersion, DeclarativeFormatVersion)
	}
	return cfg, nil
}

// toAPI validates a declared API using the same rules as the admin API. existing
// (when non-nil) supplies the stored JWT secret when the document omits it.
func toAPI(d DeclAPI, existing *store.API) (store.API, error) {
	base := store.API{
		StripPath: true, AuthType: "none", TimeoutMS: 30000, Enabled: true,
		Visibility: "public", QuotaFailurePolicy: "fail_open",
	}
	if existing != nil {
		base.ID = existing.ID
		base.CreatedAt = existing.CreatedAt
	}
	in := apiInput{
		Name: &d.Name, Description: &d.Description, BasePath: &d.BasePath, UpstreamURL: &d.UpstreamURL,
		StripPath: d.StripPath, JWKSURL: &d.JWKSURL, OIDCIssuer: &d.OIDCIssuer, OIDCAudience: &d.OIDCAudience,
		RateLimitPerMinute: &d.RateLimitPerMinute, QuotaPerDay: &d.QuotaPerDay, QuotaPerMonth: &d.QuotaPerMonth,
		CORSEnabled: &d.CORSEnabled, IsAI: &d.IsAI, RequireApproval: &d.RequireApproval, IsDraft: &d.IsDraft,
		Enabled: d.Enabled, TrafficPolicy: d.TrafficPolicy,
	}
	if d.AuthType != "" {
		in.AuthType = &d.AuthType
	}
	if d.TimeoutMS != 0 {
		in.TimeoutMS = &d.TimeoutMS
	}
	if d.Visibility != "" {
		in.Visibility = &d.Visibility
	}
	if d.QuotaFailurePolicy != "" {
		in.QuotaFailurePolicy = &d.QuotaFailurePolicy
	}
	if d.Protocol != "" {
		in.Protocol = &d.Protocol
	}
	if len(d.GRPCDescriptorSet) > 0 {
		in.GRPCDescriptorSet = &d.GRPCDescriptorSet
	}
	if d.GraphQLSchema != "" {
		in.GraphQLSchema = &d.GraphQLSchema
	}
	if d.GraphQLPolicy != nil {
		in.GraphQLPolicy = d.GraphQLPolicy
	}
	if d.GRPCPolicy != nil {
		in.GRPCPolicy = d.GRPCPolicy
	}
	if d.MCPPolicy != nil {
		in.MCPPolicy = d.MCPPolicy
	}
	headers := d.RequestHeaders
	if headers == nil {
		headers = map[string]string{}
	}
	in.RequestHeaders = &headers
	spec := d.OpenAPISpec
	if spec == nil {
		spec = map[string]any{}
	}
	in.OpenAPISpec = &spec

	secret := d.JWTSecret
	if secret != "" {
		secret = config.ResolveSecrets(secret)
		if strings.Contains(secret, "${secret:") {
			return store.API{}, fmt.Errorf("jwt_secret references a secret that is not set on the control plane (%s)", d.JWTSecret)
		}
	} else if existing != nil && existing.JWTSecret != "" {
		secret = existing.JWTSecret // secrets are never exported; keep the stored one
	}
	in.JWTSecret = &secret
	return in.apply(base)
}

func apiToDecl(a store.API) DeclAPI {
	strip, enabled := a.StripPath, a.Enabled
	proto := a.Protocol
	if proto == "" {
		proto = "http"
	}
	d := DeclAPI{
		Name: a.Name, Description: a.Description, BasePath: a.BasePath, UpstreamURL: a.UpstreamURL,
		StripPath: &strip, AuthType: a.AuthType, JWKSURL: a.JWKSURL, OIDCIssuer: a.OIDCIssuer,
		OIDCAudience: a.OIDCAudience, RateLimitPerMinute: a.RateLimitPerMinute, QuotaPerDay: a.QuotaPerDay,
		QuotaPerMonth: a.QuotaPerMonth, TimeoutMS: a.TimeoutMS, CORSEnabled: a.CORSEnabled,
		RequestHeaders: a.RequestHeaders, IsAI: a.IsAI, OpenAPISpec: a.OpenAPISpec, Visibility: a.Visibility,
		RequireApproval: a.RequireApproval, IsDraft: a.IsDraft, QuotaFailurePolicy: a.QuotaFailurePolicy,
		Protocol: proto, GraphQLSchema: a.GraphQLSchema, GRPCDescriptorSet: a.GRPCDescriptorSet,
		Enabled: &enabled,
	}
	if a.GraphQLPolicy.MaxDepth > 0 || a.GraphQLPolicy.MaxCost > 0 || a.GraphQLPolicy.AllowIntrospection {
		d.GraphQLPolicy = &a.GraphQLPolicy
	}
	if a.Protocol == "mcp" {
		d.MCPPolicy = &a.MCPPolicy
	}
	if a.GRPCPolicy.MaxMessageSizeBytes > 0 || a.GRPCPolicy.AllowReflection {
		d.GRPCPolicy = &a.GRPCPolicy
	}
	if len(d.RequestHeaders) == 0 {
		d.RequestHeaders = nil
	}
	if len(d.OpenAPISpec) == 0 {
		d.OpenAPISpec = nil
	}
	tp := a.TrafficPolicy
	tp.Normalize()
	if !reflect.DeepEqual(tp, store.TrafficPolicy{LoadBalancing: "round_robin"}) {
		d.TrafficPolicy = &tp
	}
	return d
}

func planToDecl(p store.Plan) DeclPlan {
	tier := p.Tier
	if tier == "" {
		tier = "free"
	}
	return DeclPlan{
		Name: p.Name, Description: p.Description, RateLimitPerMinute: p.RateLimitPerMinute,
		QuotaPerDay: p.QuotaPerDay, QuotaPerMonth: p.QuotaPerMonth,
		PriceMonthlyUSD: p.PriceMonthlyUSD, Tier: tier,
	}
}

// ---------------------------------------------------------------------------
// Plan computation (pure; no database access)
// ---------------------------------------------------------------------------

type FieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

type ReplaySummary struct {
	Replayed      int                `json:"replayed"`
	StatusChanged int                `json:"status_changed"`
	Examples      []SimulationResult `json:"examples,omitempty"`
}

type ResourceChange struct {
	Type     string                `json:"type"` // api | plan
	Name     string                `json:"name"`
	Action   string                `json:"action"` // create | update | delete | unchanged
	Changes  []FieldChange         `json:"changes,omitempty"`
	Warnings []string              `json:"warnings,omitempty"`
	Impact   *ConsumerImpactReport `json:"consumer_impact,omitempty"`
	Replay   *ReplaySummary        `json:"replay,omitempty"`

	existing *store.API
	proposed *store.API
}

type PlanSummary struct {
	Create    int `json:"create"`
	Update    int `json:"update"`
	Delete    int `json:"delete"`
	Unchanged int `json:"unchanged"`
}

type ConfigPlan struct {
	FormatVersion string           `json:"format_version"`
	Valid         bool             `json:"valid"`
	Errors        []string         `json:"errors,omitempty"`
	Warnings      []string         `json:"warnings,omitempty"`
	CanaryRev     int64            `json:"canary_in_flight,omitempty"`
	Prune         bool             `json:"prune"`
	HasChanges    bool             `json:"has_changes"`
	Summary       PlanSummary      `json:"summary"`
	Changes       []ResourceChange `json:"changes"`
	PlanHash      string           `json:"plan_hash"`

	apis   []store.API
	plans  []store.Plan
	tenant string
}

// ComputeConfigPlan diffs the desired document against the current state.
// subsByAPI / subsByPlan (keyed by ID) feed prune warnings and may be nil.
func ComputeConfigPlan(cfg DeclarativeConfig, current []store.API, currentPlans []store.Plan, prune bool, subsByAPI, subsByPlan map[string]int) ConfigPlan {
	return computeConfigPlan(cfg, current, currentPlans, nil, prune, subsByAPI, subsByPlan)
}

// computeConfigPlan plans changes for one tenant. current and currentPlans are
// that tenant's resources; otherPaths holds base paths used by other tenants,
// which the shared data plane makes unavailable.
func computeConfigPlan(cfg DeclarativeConfig, current []store.API, currentPlans []store.Plan, otherPaths map[string]bool, prune bool, subsByAPI, subsByPlan map[string]int) ConfigPlan {
	plan := ConfigPlan{FormatVersion: DeclarativeFormatVersion, Prune: prune, Changes: []ResourceChange{}}
	curAPIs := map[string]*store.API{}
	for i := range current {
		curAPIs[current[i].Name] = &current[i]
	}
	curPlans := map[string]store.Plan{}
	for _, p := range currentPlans {
		curPlans[p.Name] = p
	}

	// Plans.
	seenPlans := map[string]bool{}
	for i, dp := range cfg.Plans {
		tier := dp.Tier
		price := dp.PriceMonthlyUSD
		if cur, ok := curPlans[dp.Name]; ok {
			if tier == "" && cur.Tier != "" {
				tier = cur.Tier
			}
			if price == 0 && cur.PriceMonthlyUSD > 0 {
				price = cur.PriceMonthlyUSD
			}
		}
		if tier == "" {
			tier = "free"
		}
		p := store.Plan{
			Name: dp.Name, Description: dp.Description, RateLimitPerMinute: dp.RateLimitPerMinute,
			QuotaPerDay: dp.QuotaPerDay, QuotaPerMonth: dp.QuotaPerMonth,
			PriceMonthlyUSD: price, Tier: tier,
		}
		if err := validatePlan(&p); err != nil {
			plan.Errors = append(plan.Errors, fmt.Sprintf("plans[%d] %q: %v", i, dp.Name, err))
			continue
		}
		if seenPlans[p.Name] {
			plan.Errors = append(plan.Errors, fmt.Sprintf("plans[%d]: duplicate plan name %q", i, p.Name))
			continue
		}
		seenPlans[p.Name] = true
		plan.plans = append(plan.plans, p)
		ch := ResourceChange{Type: "plan", Name: p.Name}
		if cur, ok := curPlans[p.Name]; ok {
			ch.Changes = diffFields(planToDecl(cur), planToDecl(p))
			ch.Action = actionFor(ch.Changes)
		} else {
			ch.Action = "create"
		}
		plan.Changes = append(plan.Changes, ch)
	}

	// APIs.
	seenNames, seenPaths := map[string]bool{}, map[string]string{}
	for i, da := range cfg.APIs {
		label := fmt.Sprintf("apis[%d] %q", i, da.Name)
		existing := curAPIs[strings.TrimSpace(da.Name)]
		a, err := toAPI(da, existing)
		if err != nil {
			plan.Errors = append(plan.Errors, fmt.Sprintf("%s: %v", label, err))
			continue
		}
		if seenNames[a.Name] {
			plan.Errors = append(plan.Errors, fmt.Sprintf("%s: duplicate API name", label))
			continue
		}
		seenNames[a.Name] = true
		if other, ok := seenPaths[a.BasePath]; ok {
			plan.Errors = append(plan.Errors, fmt.Sprintf("%s: base_path %q is also declared by %q", label, a.BasePath, other))
			continue
		}
		if otherPaths[a.BasePath] {
			plan.Errors = append(plan.Errors, fmt.Sprintf("%s: base_path %q is already used by another tenant", label, a.BasePath))
			continue
		}
		seenPaths[a.BasePath] = a.Name
		aCopy := a
		plan.apis = append(plan.apis, a)
		ch := ResourceChange{Type: "api", Name: a.Name, proposed: &aCopy}
		if existing != nil {
			ch.existing = existing
			ch.Changes = diffFields(apiToDecl(*existing), apiToDecl(a))
			if existing.JWTSecret != a.JWTSecret {
				ch.Changes = append(ch.Changes, FieldChange{Field: "jwt_secret", Before: redactedOrEmpty(existing.JWTSecret), After: redactedOrEmpty(a.JWTSecret) + " (changed)"})
			}
			ch.Action = actionFor(ch.Changes)
		} else {
			ch.Action = "create"
		}
		plan.Changes = append(plan.Changes, ch)
	}

	// Base path collisions with existing APIs that are not declared (and not pruned).
	for _, cur := range current {
		if seenNames[cur.Name] || prune {
			continue
		}
		if owner, ok := seenPaths[cur.BasePath]; ok {
			plan.Errors = append(plan.Errors, fmt.Sprintf("api %q: base_path %q is already used by existing API %q (declare it, rename it, or apply with prune)", owner, cur.BasePath, cur.Name))
		}
	}

	// Prune: delete what the document no longer declares.
	if prune {
		for _, cur := range current {
			if seenNames[cur.Name] {
				continue
			}
			ch := ResourceChange{Type: "api", Name: cur.Name, Action: "delete"}
			if n := subsByAPI[cur.ID]; n > 0 {
				ch.Warnings = append(ch.Warnings, fmt.Sprintf("deletes %d subscription(s); those consumers lose access", n))
			}
			plan.Changes = append(plan.Changes, ch)
		}
		for _, cur := range currentPlans {
			if seenPlans[cur.Name] {
				continue
			}
			ch := ResourceChange{Type: "plan", Name: cur.Name, Action: "delete"}
			if n := subsByPlan[cur.ID]; n > 0 {
				ch.Warnings = append(ch.Warnings, fmt.Sprintf("%d subscription(s) lose this plan and fall back to API-level limits", n))
			}
			plan.Changes = append(plan.Changes, ch)
		}
	}

	for _, ch := range plan.Changes {
		switch ch.Action {
		case "create":
			plan.Summary.Create++
		case "update":
			plan.Summary.Update++
		case "delete":
			plan.Summary.Delete++
		default:
			plan.Summary.Unchanged++
		}
	}
	sort.SliceStable(plan.Changes, func(i, j int) bool {
		if plan.Changes[i].Type != plan.Changes[j].Type {
			return plan.Changes[i].Type == "plan" // plans first: APIs may reference them
		}
		return plan.Changes[i].Name < plan.Changes[j].Name
	})
	plan.Valid = len(plan.Errors) == 0
	plan.HasChanges = plan.Summary.Create+plan.Summary.Update+plan.Summary.Delete > 0
	plan.PlanHash = planHash(cfg, current, currentPlans, prune)
	return plan
}

func redactedOrEmpty(s string) string {
	if s == "" {
		return ""
	}
	return "********"
}

func actionFor(changes []FieldChange) string {
	if len(changes) == 0 {
		return "unchanged"
	}
	return "update"
}

// diffFields compares two values by their JSON representation and reports
// changed leaves with dotted paths (e.g. traffic_policy.targets[0].url).
// openapi_spec and request_headers are reported as a whole.
func diffFields(before, after any) []FieldChange {
	var out []FieldChange
	diffValue("", toMap(before), toMap(after), &out)
	return out
}

func diffValue(path string, b, a any, out *[]FieldChange) {
	if reflect.DeepEqual(b, a) {
		return
	}
	atomic := path == "openapi_spec" || path == "request_headers"
	bm, bok := b.(map[string]any)
	am, aok := a.(map[string]any)
	if bok && aok && !atomic {
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			child := k
			if path != "" {
				child = path + "." + k
			}
			diffValue(child, bm[k], am[k], out)
		}
		return
	}
	bs, bok := b.([]any)
	as, aok := a.([]any)
	if bok && aok && len(bs) == len(as) && !atomic {
		for i := range bs {
			diffValue(fmt.Sprintf("%s[%d]", path, i), bs[i], as[i], out)
		}
		return
	}
	*out = append(*out, FieldChange{Field: path, Before: b, After: a})
}

// planHash fingerprints the inputs to a plan. Apply can require the hash so a
// reviewed plan is applied only if neither the document nor the live state changed.
func planHash(cfg DeclarativeConfig, current []store.API, currentPlans []store.Plan, prune bool) string {
	type state struct {
		Config DeclarativeConfig `json:"config"`
		APIs   []DeclAPI         `json:"apis"`
		Plans  []DeclPlan        `json:"plans"`
		Prune  bool              `json:"prune"`
		Secret []string          `json:"secret_fingerprints"`
	}
	st := state{Prune: prune}
	st.Config = cfg
	st.Config.ExportedAt = ""
	for _, a := range current {
		st.APIs = append(st.APIs, apiToDecl(a))
		sum := sha256.Sum256([]byte(a.JWTSecret))
		st.Secret = append(st.Secret, a.Name+":"+hex.EncodeToString(sum[:8]))
	}
	for _, p := range currentPlans {
		st.Plans = append(st.Plans, planToDecl(p))
	}
	sort.Slice(st.APIs, func(i, j int) bool { return st.APIs[i].Name < st.APIs[j].Name })
	sort.Slice(st.Plans, func(i, j int) bool { return st.Plans[i].Name < st.Plans[j].Name })
	sort.Strings(st.Secret)
	raw, _ := json.Marshal(st)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// tenantResources returns the APIs and plans of one tenant, plus the base paths
// claimed by other tenants.
func (s *Server) tenantResources(ctx context.Context, tenant string) ([]store.API, []store.Plan, map[string]bool, error) {
	allAPIs, err := s.store.ListAPIs(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	allPlans, err := s.store.ListPlans(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	var apis []store.API
	var plans []store.Plan
	other := map[string]bool{}
	for _, a := range allAPIs {
		if store.TenantOrDefault(a.TenantID) == tenant {
			apis = append(apis, a)
		} else {
			other[a.BasePath] = true
		}
	}
	for _, p := range allPlans {
		if store.TenantOrDefault(p.TenantID) == tenant {
			plans = append(plans, p)
		}
	}
	return apis, plans, other, nil
}

func (s *Server) exportDeclarativeConfig(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	apis, plans, _, err := s.tenantResources(r.Context(), tenant)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := DeclarativeConfig{
		FormatVersion: DeclarativeFormatVersion,
		ExportedAt:    time.Now().UTC().Format(time.RFC3339),
		APIs:          make([]DeclAPI, 0, len(apis)),
		Plans:         make([]DeclPlan, 0, len(plans)),
	}
	for _, a := range apis {
		out.APIs = append(out.APIs, apiToDecl(a)) // jwt_secret is never exported
	}
	for _, p := range plans {
		out.Plans = append(out.Plans, planToDecl(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// buildConfigPlan parses the request body and computes a plan enriched with
// consumer impact and traffic replay for every API that changes.
func (s *Server) buildConfigPlan(ctx context.Context, r *http.Request) (ConfigPlan, int, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
	if err != nil {
		return ConfigPlan{}, http.StatusBadRequest, err
	}
	cfg, err := ParseDeclarativeConfig(raw)
	if err != nil {
		return ConfigPlan{Valid: false, Errors: []string{err.Error()}, Changes: []ResourceChange{}}, http.StatusBadRequest, nil
	}
	prune := r.URL.Query().Get("prune") == "true"
	tenant := scopeFrom(r).WriteTenant
	if tenant == "" {
		return ConfigPlan{Valid: false, Errors: []string{"select the tenant to plan for with the " + TenantHeader + " header"}, Changes: []ResourceChange{}}, http.StatusBadRequest, nil
	}
	apis, plans, otherPaths, err := s.tenantResources(ctx, tenant)
	if err != nil {
		return ConfigPlan{}, http.StatusInternalServerError, err
	}
	var subsByAPI, subsByPlan map[string]int
	if prune {
		subsByAPI, _ = s.store.CountSubscriptionsByAPI(ctx)
		subsByPlan, _ = s.store.CountSubscriptionsByPlan(ctx)
	}
	plan := computeConfigPlan(cfg, apis, plans, otherPaths, prune, subsByAPI, subsByPlan)
	plan.tenant = tenant
	if !plan.Valid {
		return plan, http.StatusUnprocessableEntity, nil
	}
	if canary, err := s.store.CanaryInFlight(ctx); err == nil && canary > 0 && plan.HasChanges {
		plan.CanaryRev = canary
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("canary rev_%d is in flight: apply will be refused until it is promoted or aborted", canary))
	}
	if r.URL.Query().Get("impact") != "false" {
		for i := range plan.Changes {
			ch := &plan.Changes[i]
			if ch.Type != "api" || ch.Action != "update" || ch.existing == nil || ch.proposed == nil {
				continue
			}
			impact := s.buildConsumerImpact(ctx, *ch.existing, *ch.proposed)
			ch.Impact = &impact
			if out, err := s.replayAPIChange(ctx, *ch.existing, *ch.proposed, 200); err == nil {
				sum := &ReplaySummary{Replayed: len(out.Simulations), StatusChanged: out.DiffCount}
				for _, sim := range out.Simulations {
					if sim.BehavioralDiff && len(sum.Examples) < 5 {
						sum.Examples = append(sum.Examples, sim)
					}
				}
				ch.Replay = sum
				if out.DiffCount > 0 {
					ch.Warnings = append(ch.Warnings, fmt.Sprintf("%d of %d recent requests would get a different status", out.DiffCount, len(out.Simulations)))
				}
			}
		}
	}
	return plan, http.StatusOK, nil
}

// planDeclarativeConfig is the dry run: POST /api/system/plan.
func (s *Server) planDeclarativeConfig(w http.ResponseWriter, r *http.Request) {
	plan, status, err := s.buildConfigPlan(r.Context(), r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, status, plan)
}

// applyDeclarativeConfig validates, plans and atomically applies the document as one
// revision. Query parameters:
//
//	prune=true                 delete APIs/plans the document does not declare
//	plan_hash=<hash>           refuse (409) unless the plan still has this hash
//	rollout=canary             publish as the in-flight canary instead of fleet-wide
//	traffic_percent=N          canary traffic share on standard nodes (with rollout=canary)
//	canary_header=Name[&canary_header_value=V]  route matching requests to the canary
func (s *Server) applyDeclarativeConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	plan, status, err := s.buildConfigPlan(ctx, r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !plan.Valid {
		writeJSON(w, status, map[string]any{"error": "validation_failed", "message": strings.Join(plan.Errors, "; "), "plan": plan})
		return
	}
	q := r.URL.Query()
	if want := q.Get("plan_hash"); want != "" && want != plan.PlanHash {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "plan_stale",
			"message": "the configuration or the live state changed since the plan was reviewed; re-run plan",
			"plan":    plan,
		})
		return
	}
	if expectedBase := q.Get("expected_base_revision"); expectedBase != "" {
		baseRev, _ := strconv.ParseInt(expectedBase, 10, 64)
		if curRev, err := s.store.GetLatestRevision(ctx); err == nil && baseRev > 0 && curRev != baseRev {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "base_revision_mismatch",
				"message": fmt.Sprintf("expected base revision %d but live revision is %d; re-plan against live state", baseRev, curRev),
			})
			return
		}
	}
	if !plan.HasChanges {
		writeJSON(w, http.StatusOK, map[string]any{"revision": 0, "applied": false, "message": "No changes. Live configuration already matches the document.", "plan": plan})
		return
	}

	var split store.CanarySplit
	rollout := q.Get("rollout")
	switch rollout {
	case "", "all":
		rollout = "all"
		var changedAPIIDs []string
		for _, a := range plan.apis {
			if a.ID != "" {
				changedAPIIDs = append(changedAPIIDs, a.ID)
			}
		}
		for _, ch := range plan.Changes {
			if ch.Type == "api" && ch.existing != nil && ch.existing.ID != "" {
				changedAPIIDs = append(changedAPIIDs, ch.existing.ID)
			}
		}
		hasGate, err := s.store.HasEnforcedTestGates(ctx, plan.tenant, changedAPIIDs...)
		if err != nil {
			s.fail(w, err)
			return
		}
		if hasGate {
			u, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
			overrideReason := q.Get("override_reason")
			if overrideReason == "" {
				overrideReason = r.Header.Get("X-RelayOps-Override-Reason")
			}
			if u.Role != "superadmin" || strings.TrimSpace(overrideReason) == "" {
				writeJSON(w, http.StatusPreconditionFailed, map[string]any{
					"error":   "promotion_gate_required",
					"message": "one or more modified APIs have active test gate policies: direct all-fleet apply is blocked. Deploy as a canary (rollout=canary) and pass verification suites, or supply superadmin override.",
				})
				return
			}
		}
	case "canary":
		split.TrafficPercent, _ = strconv.Atoi(q.Get("traffic_percent"))
		split.Header = q.Get("canary_header")
		split.HeaderValue = q.Get("canary_header_value")
		if err := validateSplit(&split); err != nil {
			writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "validation_failed", "rollout must be 'all' or 'canary'")
		return
	}

	mutate := func(tx pgx.Tx) error {
		if want := q.Get("plan_hash"); want != "" && want != plan.PlanHash {
			return fmt.Errorf("%w: plan_hash mismatch inside publication transaction (reviewed %s, computed %s)", store.ErrConflict, want, plan.PlanHash)
		}
		if expectedBase := q.Get("expected_base_revision"); expectedBase != "" {
			baseRev, _ := strconv.ParseInt(expectedBase, 10, 64)
			var curRev int64
			if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(revision), 0) FROM config_revisions WHERE status='active'`).Scan(&curRev); err != nil {
				return err
			} else if baseRev > 0 && curRev != baseRev {
				return fmt.Errorf("%w: concurrent publication occurred (expected revision %d, current is %d)", store.ErrConflict, baseRev, curRev)
			}
		}
		for _, ch := range plan.Changes {
			if ch.Action != "delete" {
				continue
			}
			var err error
			if ch.Type == "api" {
				err = store.DeleteAPIByNameTx(ctx, tx, plan.tenant, ch.Name)
			} else {
				err = store.DeletePlanByNameTx(ctx, tx, plan.tenant, ch.Name)
			}
			if err != nil {
				return err
			}
		}
		for _, p := range plan.plans {
			p.TenantID = plan.tenant
			if _, err := store.UpsertPlanByNameTx(ctx, tx, p); err != nil {
				return err
			}
		}
		for _, a := range plan.apis {
			a.TenantID = plan.tenant
			if _, err := store.UpsertAPIByNameTx(ctx, tx, a); err != nil {
				return err
			}
		}
		return nil
	}

	actor := s.getActor(r)
	desc := fmt.Sprintf("Declarative apply: %d create, %d update, %d delete", plan.Summary.Create, plan.Summary.Update, plan.Summary.Delete)
	var rev int64
	if rollout == "canary" {
		rev, err = s.store.AtomicPublishCanary(ctx, actor, desc+" (canary)", split, mutate)
	} else {
		rev, err = s.store.AtomicPublishConfig(ctx, actor, desc, "all", "active", mutate)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, plan.tenant, "DECLARATIVE_APPLY", "system", strconv.FormatInt(rev, 10), map[string]any{
		"revision": rev, "rollout": rollout, "prune": plan.Prune, "plan_hash": plan.PlanHash,
		"create": plan.Summary.Create, "update": plan.Summary.Update, "delete": plan.Summary.Delete,
		"traffic_percent": split.TrafficPercent, "canary_header": split.Header,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"revision":      rev,
		"applied":       true,
		"rollout":       rollout,
		"apis_applied":  len(plan.apis),
		"plans_applied": len(plan.plans),
		"plan":          plan,
		"message":       "Declarative configuration applied atomically as revision " + strconv.FormatInt(rev, 10) + ".",
	})
}
