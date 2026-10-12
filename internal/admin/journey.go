package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/relayops/apim/internal/store"
)

type journeyStep struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

func (s *Server) listReleaseDrafts(w http.ResponseWriter, r *http.Request) {
	ds, err := s.store.ListReleaseDrafts(r.Context(), scopeFrom(r).filter())
	s.respond(w, 200, ds, err)
}
func (s *Server) scopedDraft(w http.ResponseWriter, r *http.Request) (store.ReleaseDraft, bool) {
	d, err := s.store.GetReleaseDraft(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return d, false
	}
	if !scopeFrom(r).allows(d.TenantID) {
		writeErr(w, 403, "forbidden", "draft access denied")
		return d, false
	}
	return d, true
}
func (s *Server) saveReleaseDraft(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in struct {
		Version       int             `json:"version"`
		Configuration json.RawMessage `json:"configuration"`
		Spec          string          `json:"spec"`
		APIID         string          `json:"api_id"`
		SuiteIDs      []string        `json:"suite_ids"`
		EnvironmentID *string         `json:"environment_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Spec != "" {
		imported, err := ParseOpenAPISpec([]byte(in.Spec), true)
		if err != nil {
			writeErr(w, 400, "invalid_spec", err.Error())
			return
		}
		a := imported.API
		a.IsDraft = false
		a.Enabled = true
		in.Configuration, _ = json.Marshal(DeclarativeConfig{FormatVersion: "1.0", APIs: []DeclAPI{apiToDecl(a)}, Plans: []DeclPlan{}})
	}
	if in.APIID != "" {
		a, ok := s.scopedAPI(w, r, in.APIID)
		if !ok {
			return
		}
		tenant = a.TenantID
		if a.Protocol != "" && a.Protocol != "http" {
			writeErr(w, 400, "unsupported_protocol", "use the advanced workflow for this protocol")
			return
		}
		a.IsDraft = false
		a.Enabled = true
		in.Configuration, _ = json.Marshal(DeclarativeConfig{FormatVersion: "1.0", APIs: []DeclAPI{apiToDecl(a)}, Plans: []DeclPlan{}})
	}
	cfg, err := ParseDeclarativeConfig(in.Configuration)
	if err != nil || len(cfg.APIs) != 1 || len(cfg.Plans) > 0 {
		writeErr(w, 400, "invalid_draft", "a release draft must contain exactly one HTTP API and no plan mutations")
		return
	}
	if cfg.APIs[0].Protocol != "" && cfg.APIs[0].Protocol != "http" {
		writeErr(w, 400, "unsupported_protocol", "guided releases support HTTP APIs")
		return
	}
	if err := draftSecretReferences(cfg); err != nil {
		writeErr(w, 400, "secret_reference_required", err.Error())
		return
	}
	d := store.ReleaseDraft{TenantID: tenant, Configuration: in.Configuration, SuiteIDs: in.SuiteIDs, EnvironmentID: in.EnvironmentID, CreatedBy: s.getActor(r)}
	if r.PathValue("id") != "" {
		current, ok := s.scopedDraft(w, r)
		if !ok {
			return
		}
		if current.TenantID != tenant {
			writeErr(w, 403, "forbidden", "select the draft tenant before saving")
			return
		}
		d.ID = current.ID
		d.Version = in.Version
	}
	if err := s.validateDraftTests(r.Context(), d); err != nil {
		writeErr(w, 400, "invalid_tests", err.Error())
		return
	}
	saved, err := s.store.SaveReleaseDraft(r.Context(), d)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, tenant, "RELEASE_DRAFT_SAVE", "release_draft", saved.ID, map[string]any{"version": saved.Version})
	writeJSON(w, 200, saved)
}

// Reject inline credentials, including examples/defaults in specifications.
func draftSecretReferences(cfg DeclarativeConfig) error {
	for _, a := range cfg.APIs {
		upstream, err := url.Parse(a.UpstreamURL)
		if err == nil && upstream.User != nil {
			return errors.New("upstream URLs cannot contain credentials; use secret-referenced request headers")
		}
		if err == nil {
			for key, values := range upstream.Query() {
				if sensitiveHeader(key) || strings.EqualFold(key, "password") {
					for _, value := range values {
						if !secretReference(value) {
							return errors.New("upstream credential parameters must use secret references")
						}
					}
				}
			}
		}
		if a.JWTSecret != "" && !secretReference(a.JWTSecret) {
			return errors.New("JWT secrets must use ${secret:NAME}")
		}
		for k, v := range a.RequestHeaders {
			if sensitiveHeader(k) && !secretReference(v) {
				return fmt.Errorf("header %s must use a secret reference", k)
			}
		}
		raw, _ := json.Marshal(a.OpenAPISpec)
		var doc any
		_ = json.Unmarshal(raw, &doc)
		if hasInlineSecret(doc) {
			return errors.New("remove credential values from the API specification")
		}
	}
	return nil
}

var draftSecretPattern = regexp.MustCompile(`^(?:(?:Bearer|Basic) )?\$\{secret:[A-Za-z0-9_.-]+\}$`)

func secretReference(v string) bool { return draftSecretPattern.MatchString(v) }
func sensitiveHeader(k string) bool {
	k = strings.ReplaceAll(strings.ToLower(k), "_", "-")
	return strings.Contains(k, "authorization") || strings.Contains(k, "cookie") || strings.Contains(k, "api-key") || strings.Contains(k, "apikey") || strings.Contains(k, "secret") || strings.Contains(k, "token")
}
func hasInlineSecret(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if sensitiveHeader(k) || strings.EqualFold(k, "password") {
				if s, ok := v.(string); ok && s != "" && !secretReference(s) {
					return true
				}
			}
			if hasInlineSecret(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if hasInlineSecret(v) {
				return true
			}
		}
	}
	return false
}
func (s *Server) validateDraftTests(ctx context.Context, d store.ReleaseDraft) error {
	if d.EnvironmentID != nil {
		env, err := s.store.GetTestEnvironment(ctx, *d.EnvironmentID)
		if err != nil || env.TenantID != d.TenantID {
			return errors.New("test environment does not belong to draft tenant")
		}
	}
	for _, id := range d.SuiteIDs {
		suite, err := s.store.GetTestSuite(ctx, id)
		if err != nil || suite.TenantID != d.TenantID {
			return errors.New("test suite does not belong to draft tenant")
		}
	}
	return nil
}
func draftRequest(r *http.Request, d store.ReleaseDraft) *http.Request {
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(d.Configuration))
	clone.ContentLength = int64(len(d.Configuration))
	scope := scopeFrom(r)
	scope.WriteTenant = d.TenantID
	return clone.WithContext(context.WithValue(clone.Context(), scopeCtxKey{}, scope))
}
func (s *Server) planReleaseDraft(w http.ResponseWriter, r *http.Request) {
	d, ok := s.scopedDraft(w, r)
	if !ok {
		return
	}
	if d.CandidateRevision > 0 {
		writeErr(w, 409, "already_deployed", "create a new draft to change a deployed release")
		return
	}
	plan, status, err := s.buildConfigPlan(r.Context(), draftRequest(r, d))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !plan.Valid {
		writeJSON(w, status, plan)
		return
	}
	base, _, err := s.store.GetRolloutState(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	tag, err := s.store.Pool.Exec(r.Context(), `UPDATE release_drafts SET plan_hash=$3,baseline_revision=$4,updated_at=now() WHERE id=$1 AND version=$2 AND candidate_revision=0`, d.ID, d.Version, plan.PlanHash, base)
	if err != nil {
		s.fail(w, err)
		return
	}
	if tag.RowsAffected() != 1 {
		writeErr(w, 409, "draft_stale", "reload the changed draft")
		return
	}
	writeJSON(w, 200, map[string]any{"plan": plan, "baseline_revision": base, "version": d.Version})
}

func (s *Server) canaryReleaseDraft(w http.ResponseWriter, r *http.Request) {
	if !platformReleaseAdmin(r) {
		writeErr(w, 403, "platform_only", "canary deployment affects the fleet and requires a platform administrator")
		return
	}
	d, ok := s.scopedDraft(w, r)
	if !ok {
		return
	}
	if d.CandidateRevision > 0 {
		writeJSON(w, 200, map[string]any{"revision": d.CandidateRevision, "applied": true})
		return
	}
	var in struct {
		Version        int    `json:"version"`
		PlanHash       string `json:"plan_hash"`
		TrafficPercent int    `json:"traffic_percent"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Version != d.Version || in.PlanHash == "" || in.PlanHash != d.PlanHash {
		writeErr(w, 409, "plan_stale", "review the current draft before deploying")
		return
	}
	if len(d.SuiteIDs) == 0 || d.EnvironmentID == nil {
		writeErr(w, 412, "tests_required", "select required suites and a test environment before deployment")
		return
	}
	if err := s.validateDraftTests(r.Context(), d); err != nil {
		writeErr(w, 412, "invalid_tests", err.Error())
		return
	}
	baselineRecorded, err := s.baselineDraftRecorded(r.Context(), d, d.BaselineRevision)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !baselineRecorded {
		writeErr(w, 412, "baseline_required", "run the selected baseline checks and review their completed results before launching the candidate")
		return
	}
	if in.TrafficPercent == 0 {
		in.TrafficPercent = 10
	}
	if in.TrafficPercent < 1 || in.TrafficPercent > 50 {
		writeErr(w, 400, "invalid_exposure", "guided canary exposure must be between 1 and 50 percent")
		return
	}
	clone := draftRequest(r, d)
	u := *clone.URL
	clone.URL = &u
	clone.URL.RawQuery = "rollout=canary&traffic_percent=" + strconv.Itoa(in.TrafficPercent) + "&expected_base_revision=" + strconv.FormatInt(d.BaselineRevision, 10) + "&plan_hash=" + d.PlanHash
	clone = clone.WithContext(store.WithReleaseDraft(clone.Context(), d))
	s.applyDeclarativeConfig(w, clone)
}

// Called from declarative publication's transaction after API upserts.
func installDraftGates(ctx context.Context, tx pgx.Tx, d store.ReleaseDraft) error {
	var cfg DeclarativeConfig
	if err := json.Unmarshal(d.Configuration, &cfg); err != nil {
		return err
	}
	var apiID string
	if err := tx.QueryRow(ctx, `SELECT id FROM apis WHERE tenant_id=$1 AND name=$2`, d.TenantID, cfg.APIs[0].Name).Scan(&apiID); err != nil {
		return err
	}
	for _, id := range d.SuiteIDs {
		tag, err := tx.Exec(ctx, `UPDATE test_suites SET api_id=$3 WHERE id=$1 AND tenant_id=$2 AND (api_id IS NULL OR api_id=$3)`, id, d.TenantID, apiID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: required suite belongs to a different API", store.ErrConflict)
		}
	}
	suites, _ := json.Marshal(d.SuiteIDs)
	_, err := tx.Exec(ctx, `INSERT INTO test_gate_policies(tenant_id,api_id,target_environment,required_suite_ids,freshness_seconds,enforcement_enabled) VALUES($1,$2,'canary',$3,3600,true) ON CONFLICT(tenant_id,api_id,target_environment) DO UPDATE SET required_suite_ids=(SELECT jsonb_agg(DISTINCT value) FROM jsonb_array_elements(test_gate_policies.required_suite_ids || $3::jsonb)),enforcement_enabled=true,updated_at=now()`, d.TenantID, apiID, suites)
	return err
}

func (s *Server) releaseJourney(w http.ResponseWriter, r *http.Request) {
	d, ok := s.scopedDraft(w, r)
	if !ok {
		return
	}
	// Fleet-wide gate evidence is visible only to platform administrators.
	if !scopeFrom(r).Platform && d.CandidateRevision > 0 {
		writeErr(w, 403, "platform_only", "fleet review requires a platform administrator")
		return
	}
	steps := []journeyStep{{"Import", "passed", "Draft saved; live traffic is unchanged"}, {"Protect", "in_progress", "Review authentication and limits"}, {"Test", "not_started", "Select suites and an environment"}, {"Review change", "not_started", "Review the current plan"}, {"Canary", "not_started", "Candidate has not been deployed"}, {"Release decision", "not_started", "Fresh candidate evidence is required"}, {"Evidence", "not_started", "Release outcome pending"}}
	cfg, _ := ParseDeclarativeConfig(d.Configuration)
	if len(cfg.APIs) == 1 {
		a := cfg.APIs[0]
		if a.AuthType != "" && (a.RateLimitPerMinute > 0 || a.QuotaPerDay > 0) {
			steps[1] = journeyStep{"Protect", "passed", "Authentication and traffic policy configured"}
		}
	}
	if len(d.SuiteIDs) > 0 && d.EnvironmentID != nil {
		steps[2] = journeyStep{"Test", "in_progress", "Required suites selected; run baseline tests before deployment"}
		base, _, err := s.store.GetRolloutState(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		complete, err := s.baselineDraftRecorded(r.Context(), d, base)
		if err != nil {
			s.fail(w, err)
			return
		}
		if complete {
			steps[2] = journeyStep{"Test", "passed", "Baseline results recorded; candidate checks are still required after deployment"}
		}
	}
	if d.PlanHash != "" {
		steps[3] = journeyStep{"Review change", "passed", "Plan available for explicit review"}
	}
	var report *store.GateReport
	outcome := "draft"
	var fleet *store.FleetStatus
	var blockers []string
	if d.CandidateRevision == 0 && d.PlanHash != "" {
		plan, _, err := s.buildConfigPlan(r.Context(), draftRequest(r, d))
		if err != nil {
			s.fail(w, err)
			return
		}
		base, _, err := s.store.GetRolloutState(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if plan.PlanHash != d.PlanHash || base != d.BaselineRevision {
			steps[3] = journeyStep{"Review change", "stale", "Live configuration changed; review a fresh plan"}
		}
	}
	if d.CandidateRevision > 0 {
		var err error
		rep, e := s.store.EvaluateTestGates(r.Context(), d.CandidateRevision)
		if e != nil {
			s.fail(w, e)
			return
		}
		report = &rep
		rev, e := s.store.GetConfigRevisionMeta(r.Context(), d.CandidateRevision)
		if e != nil {
			s.fail(w, e)
			return
		}
		outcome = rev.Status
		if d.DeploymentID != nil && rev.Status == "rolled_back" {
			deployment, err := s.store.GetAPIOpsDeployment(r.Context(), *d.DeploymentID)
			if err != nil {
				s.fail(w, err)
				return
			}
			if deployment.Status == "aborted" {
				outcome = "aborted"
			}
		}
		steps[4] = journeyStep{"Canary", "in_progress", "Candidate deployed; check fleet acknowledgement"}
		steps[2] = journeyStep{"Test", "blocked", "Candidate evidence is missing or stale"}
		if rep.Enforced && rep.Eligible {
			steps[2] = journeyStep{"Test", "passed", "Required candidate evidence is fresh and passing"}
		}
		blockers, err = s.releaseTrafficBlockers(r.Context(), d)
		if err != nil {
			s.fail(w, err)
			return
		}
		blockers = append(blockers, rep.Reasons...)
		if !rep.Enforced {
			blockers = append(blockers, "Required test gates are not configured")
		}
		steps[5] = journeyStep{"Release decision", "blocked", strings.Join(blockers, "; ")}
		if len(blockers) == 0 {
			steps[5] = journeyStep{"Release decision", "in_progress", "Ready for administrator review"}
			steps[4].State = "passed"
		}
		if outcome != "canary" {
			steps[5] = journeyStep{"Release decision", "passed", "Outcome: " + outcome}
			steps[6] = journeyStep{"Evidence", "passed", "Export the recorded outcome and evidence"}
		}
		f, e := s.store.GetFleetStatus(r.Context())
		if e == nil {
			fleet = &f
		}
	}
	writeJSON(w, 200, map[string]any{"draft": d, "steps": steps, "gate_status": report, "blockers": blockers, "outcome": outcome, "fleet": fleet, "can_promote": outcome == "canary" && len(blockers) == 0, "can_abort": outcome == "canary"})
}
func (s *Server) releaseTrafficBlockers(ctx context.Context, d store.ReleaseDraft) ([]string, error) {
	settings, err := s.store.GetAutoRollbackSettings(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := s.store.RevisionErrorStats(ctx, time.Duration(settings.EvaluationWindowSeconds)*time.Second, d.CandidateRevision, d.BaselineRevision)
	if err != nil {
		return nil, err
	}
	b := []string{}
	candidate := stats[d.CandidateRevision]
	baseline := stats[d.BaselineRevision]
	if !settings.Enabled {
		b = append(b, "Enable automatic rollback before promotion")
	}
	if candidate.Requests < int64(settings.MinRequests) {
		b = append(b, "Not enough candidate traffic to assess health")
	}
	if baseline.Requests < int64(settings.MinBaselineRequests) {
		b = append(b, "Not enough baseline traffic for comparison")
	}
	if candidate.Requests > 0 && float64(candidate.Errors)*100/float64(candidate.Requests) >= settings.ErrorRateThresholdPercent {
		b = append(b, "Candidate error rate exceeds the rollback threshold")
	}
	fleet, err := s.store.GetFleetStatus(ctx)
	if err != nil {
		return nil, err
	}
	if len(fleet.Nodes) == 0 || !fleet.Converged {
		b = append(b, "Gateway fleet has not acknowledged the release")
	}
	for _, n := range fleet.Nodes {
		if n.Revision != d.CandidateRevision && n.CanaryRev != d.CandidateRevision {
			b = append(b, "A gateway has not acknowledged the candidate")
			break
		}
	}
	return b, nil
}
func (s *Server) decideReleaseDraft(w http.ResponseWriter, r *http.Request) {
	if !platformReleaseAdmin(r) {
		writeErr(w, 403, "platform_only", "release decisions require a platform administrator")
		return
	}
	d, ok := s.scopedDraft(w, r)
	if !ok {
		return
	}
	if d.DeploymentID == nil {
		writeErr(w, 412, "candidate_required", "deploy a candidate first")
		return
	}
	var in struct {
		Reviewed bool `json:"reviewed"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Reviewed {
		writeErr(w, 412, "review_required", "review the release before confirming")
		return
	}
	action := r.PathValue("action")
	if action == "promote" {
		dep, err := s.store.GetAPIOpsDeployment(r.Context(), *d.DeploymentID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if dep.Status == "promoted" {
			pass, err := s.store.GetAPIOpsReleasePassport(r.Context(), dep.ID)
			s.respond(w, 200, pass, err)
			return
		}
		blockers, err := s.releaseTrafficBlockers(r.Context(), d)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(blockers) > 0 {
			writeErr(w, 412, "health_gate_failed", strings.Join(blockers, "; "))
			return
		}
		pass, err := s.apiops.PromoteDeployment(r.Context(), dep.ID, s.getActor(r), false, "")
		if err != nil {
			writeErr(w, 412, "promotion_blocked", err.Error())
			return
		}
		s.auditTenant(r, d.TenantID, "GUIDED_PROMOTE", "release_draft", d.ID, map[string]any{"passport_id": pass.ID})
		writeJSON(w, 200, pass)
	} else if action == "abort" {
		dep, err := s.store.GetAPIOpsDeployment(r.Context(), *d.DeploymentID)
		if err != nil {
			s.fail(w, err)
			return
		}
		if dep.Status != "aborted" {
			if err := s.apiops.AbortDeployment(r.Context(), dep.ID, s.getActor(r)); err != nil {
				s.fail(w, err)
				return
			}
		}
		s.auditTenant(r, d.TenantID, "GUIDED_ABORT", "release_draft", d.ID, nil)
		writeJSON(w, 200, map[string]string{"status": "aborted"})
	} else {
		writeErr(w, 404, "not_found", "unknown release decision")
	}
}
func (s *Server) exportReleaseDraft(w http.ResponseWriter, r *http.Request) {
	if !platformReleaseAdmin(r) {
		writeErr(w, 403, "platform_only", "fleet evidence export requires a platform administrator")
		return
	}
	d, ok := s.scopedDraft(w, r)
	if !ok {
		return
	}
	out := map[string]any{"draft_id": d.ID, "tenant_id": d.TenantID, "baseline_revision": d.BaselineRevision, "candidate_revision": d.CandidateRevision, "plan_hash": d.PlanHash}
	fleet, err := s.store.GetFleetStatus(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out["fleet"] = map[string]any{"converged": len(fleet.Nodes) > 0 && fleet.Converged, "node_count": len(fleet.Nodes), "target_revision": fleet.TargetRevision}
	if d.DeploymentID != nil {
		dep, err := s.store.GetAPIOpsDeployment(r.Context(), *d.DeploymentID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["outcome"] = dep.Status
		if dep.Status == "promoted" {
			pass, err := s.store.GetAPIOpsReleasePassport(r.Context(), dep.ID)
			if err != nil {
				s.fail(w, err)
				return
			}
			out["passport"] = pass
		}
	}
	if d.CandidateRevision > 0 {
		rev, err := s.store.GetConfigRevisionMeta(r.Context(), d.CandidateRevision)
		if err != nil {
			s.fail(w, err)
			return
		}
		out["revision_status"] = rev.Status
	}
	s.auditTenant(r, d.TenantID, "RELEASE_EVIDENCE_EXPORT", "release_draft", d.ID, nil)
	w.Header().Set("Content-Disposition", `attachment; filename="release-evidence.json"`)
	writeJSON(w, 200, out)
}

func platformReleaseAdmin(r *http.Request) bool {
	u, _ := r.Context().Value(userCtxKey{}).(store.AdminUser)
	return scopeFrom(r).Platform && (u.Role == "superadmin" || u.Role == "admin")
}

func (s *Server) baselineDraftRecorded(ctx context.Context, d store.ReleaseDraft, base int64) (bool, error) {
	if d.EnvironmentID == nil || len(d.SuiteIDs) == 0 {
		return false, nil
	}
	for _, id := range d.SuiteIDs {
		var found bool
		err := s.store.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM test_runs r JOIN test_suites s ON s.id=r.suite_id JOIN test_suite_versions v ON v.suite_id=s.id AND v.version=s.current_version JOIN test_environments e ON e.id=r.environment_id WHERE r.tenant_id=$1 AND r.suite_id=$2 AND r.environment_id=$3 AND r.actual_revision=$4 AND r.lifecycle_state IN ('completed','failed') AND r.completed_at IS NOT NULL AND r.total_steps>0 AND r.passed_steps+r.failed_steps+r.skipped_steps=r.total_steps AND r.suite_content_hash=v.content_hash AND r.immutable_inputs->'environment_snapshot'->>'revision'=e.revision::text)`, d.TenantID, id, *d.EnvironmentID, base).Scan(&found)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

func (s *Server) requireReleaseReview(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !platformReleaseAdmin(r) {
			writeErr(w, 403, "platform_only", "promotion requires a platform administrator")
			return
		}
		if r.Body == nil {
			writeErr(w, 412, "review_required", "send reviewed=true after reviewing the release")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeErr(w, 400, "invalid_review", "cannot read review confirmation")
			return
		}
		var in struct {
			Reviewed bool `json:"reviewed"`
		}
		if json.Unmarshal(raw, &in) != nil || !in.Reviewed {
			writeErr(w, 412, "review_required", "review the latest evidence and send reviewed=true to confirm promotion")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		next(w, r)
	}
}

func (s *Server) exportRevisionEvidence(w http.ResponseWriter, r *http.Request) {
	if !platformReleaseAdmin(r) {
		writeErr(w, 403, "platform_only", "fleet evidence export requires a platform administrator")
		return
	}
	rev, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid_revision", "use an integer revision")
		return
	}
	var raw json.RawMessage
	var digest string
	var at time.Time
	err = s.store.Pool.QueryRow(r.Context(), `SELECT evidence,evidence_digest,created_at FROM release_decisions WHERE revision=$1`, rev).Scan(&raw, &digest, &at)
	if err == pgx.ErrNoRows {
		writeErr(w, 412, "evidence_unavailable", "no authoritative promotion decision was recorded for this revision")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "RELEASE_EVIDENCE_EXPORT", "revision", r.PathValue("id"), nil)
	writeJSON(w, 200, map[string]any{"revision": rev, "gate_report": raw, "evidence_digest": digest, "promoted_at": at})
}
