package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
	"github.com/relayops/apim/internal/testingstudio"
	"gopkg.in/yaml.v3"
)

// StartTestRunner launches the durable background test execution worker.
func (s *Server) StartTestRunner(ctx context.Context) {
	cfg := testingstudio.RunnerConfig{
		WorkerID:        fmt.Sprintf("worker-%s-%d", s.nodeID, os.Getpid()),
		RunnerToken:     s.runnerToken,
		TrustedGateways: []string{s.gatewayURL, "http://127.0.0.1:8080", "http://localhost:8080"},
	}
	runner := testingstudio.NewRunner(s.store, cfg)
	runner.Start()
	slog.Info("Test Studio background runner started", "node_id", s.nodeID, "worker_id", cfg.WorkerID)
	<-ctx.Done()
	runner.Stop()
}

// StartTestRetentionCleaner purges expired steps (>7 days) and runs (>30 days).
func (s *Server) StartTestRetentionCleaner(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purged, err := s.store.PurgeExpiredTestStudioData(ctx)
			if err != nil {
				slog.Warn("Test Studio retention cleaner error", "error", err)
			} else if purged > 0 {
				slog.Info("Test Studio retention cleaner purged records", "purged_runs", purged)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Test Suites
// ---------------------------------------------------------------------------

type suiteCreateReq struct {
	TenantID    string                `json:"tenant_id"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	APIID       string                `json:"api_id"`
	Ownership   string                `json:"ownership"`
	Visibility  string                `json:"visibility"`
	Definition  store.SuiteDefinition `json:"definition"`
}

func (s *Server) listTestSuites(w http.ResponseWriter, r *http.Request) {
	sc := scopeFrom(r)
	tenantID := ""
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}
	if qTenant := r.URL.Query().Get("tenant"); qTenant != "" && sc.allows(qTenant) {
		tenantID = qTenant
	}

	suites, err := s.store.ListTestSuites(r.Context(), tenantID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, suites)
}

func (s *Server) createTestSuite(w http.ResponseWriter, r *http.Request) {
	var req suiteCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse request JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "test suite name is required")
		return
	}

	def := req.Definition
	if def.Name == "" {
		def.Name = req.Name
	}
	if def.Description == "" {
		def.Description = req.Description
	}

	if err := testingstudio.ValidateSuiteDefinition(def); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_definition", err.Error())
		return
	}

	sc := scopeFrom(r)
	tenantID := store.DefaultTenantID
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}
	if req.TenantID != "" {
		if !sc.allows(req.TenantID) {
			writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
			return
		}
		tenantID = req.TenantID
	}

	var apiIDPtr *string
	if trimmed := strings.TrimSpace(req.APIID); trimmed != "" {
		apiIDPtr = &trimmed
	}

	ownership := req.Ownership
	if ownership == "" {
		ownership = "team"
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = "tenant"
	}

	suite := store.TestSuite{
		TenantID:    tenantID,
		Name:        strings.TrimSpace(req.Name),
		Description: strings.TrimSpace(req.Description),
		APIID:       apiIDPtr,
		Ownership:   ownership,
		Visibility:  visibility,
		CreatedBy:   s.getActor(r),
	}

	created, version, err := s.store.CreateTestSuite(r.Context(), suite, def, s.getActor(r))
	if err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "CREATE_TEST_SUITE", "test_suite", created.ID, map[string]any{"name": created.Name, "version": version.Version})
	writeJSON(w, http.StatusCreated, map[string]any{
		"suite":   created,
		"version": version,
	})
}

func (s *Server) getTestSuite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	suite, err := s.store.GetTestSuite(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(suite.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	version, err := s.store.GetLatestTestSuiteVersion(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"suite":   suite,
		"version": version,
	})
}

func (s *Server) updateTestSuite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	suite, err := s.store.GetTestSuite(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(suite.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	var req struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description"`
		APIID       string                 `json:"api_id"`
		Ownership   string                 `json:"ownership"`
		Visibility  string                 `json:"visibility"`
		Definition  *store.SuiteDefinition `json:"definition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse request JSON: "+err.Error())
		return
	}

	if req.Name != "" {
		suite.Name = strings.TrimSpace(req.Name)
	}
	suite.Description = strings.TrimSpace(req.Description)
	if req.APIID != "" {
		trimmed := strings.TrimSpace(req.APIID)
		suite.APIID = &trimmed
	}
	if req.Ownership != "" {
		suite.Ownership = req.Ownership
	}
	if req.Visibility != "" {
		suite.Visibility = req.Visibility
	}

	if req.Definition != nil {
		if req.Definition.Name == "" {
			req.Definition.Name = suite.Name
		}
		if err := testingstudio.ValidateSuiteDefinition(*req.Definition); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_definition", err.Error())
			return
		}
	}

	updated, newVer, err := s.store.UpdateTestSuite(r.Context(), suite, req.Definition, s.getActor(r))
	if err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "UPDATE_TEST_SUITE", "test_suite", updated.ID, map[string]any{"name": updated.Name})
	writeJSON(w, http.StatusOK, map[string]any{
		"suite":   updated,
		"version": newVer,
	})
}

func (s *Server) deleteTestSuite(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	suite, err := s.store.GetTestSuite(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(suite.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	if err := s.store.DeleteTestSuite(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "DELETE_TEST_SUITE", "test_suite", id, map[string]any{"name": suite.Name})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) listTestSuiteVersions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	suite, err := s.store.GetTestSuite(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(suite.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	versions, err := s.store.ListTestSuiteVersions(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, versions)
}

// ---------------------------------------------------------------------------
// Test Environments
// ---------------------------------------------------------------------------

func (s *Server) listTestEnvironments(w http.ResponseWriter, r *http.Request) {
	sc := scopeFrom(r)
	tenantID := ""
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}
	if qTenant := r.URL.Query().Get("tenant"); qTenant != "" && sc.allows(qTenant) {
		tenantID = qTenant
	}

	envs, err := s.store.ListTestEnvironments(r.Context(), tenantID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envs)
}

func (s *Server) createTestEnvironment(w http.ResponseWriter, r *http.Request) {
	var env store.TestEnvironment
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse request JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(env.Name) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "environment name is required")
		return
	}

	if env.GatewayTarget != "" {
		if _, err := testingstudio.ValidateGatewayTarget(env.GatewayTarget); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_gateway_target", err.Error())
			return
		}
	}

	sc := scopeFrom(r)
	if env.TenantID == "" {
		if !sc.All && len(sc.TenantIDs) > 0 {
			env.TenantID = sc.TenantIDs[0]
		} else {
			env.TenantID = store.DefaultTenantID
		}
	} else if !sc.allows(env.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	created, err := s.store.CreateTestEnvironment(r.Context(), env)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "CREATE_TEST_ENVIRONMENT", "test_environment", created.ID, map[string]any{"name": created.Name})
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) updateTestEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	env, err := s.store.GetTestEnvironment(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(env.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	var req store.TestEnvironment
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse request JSON: "+err.Error())
		return
	}

	if req.Name != "" {
		env.Name = strings.TrimSpace(req.Name)
	}
	if req.GatewayTarget != "" {
		if _, err := testingstudio.ValidateGatewayTarget(req.GatewayTarget); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_gateway_target", err.Error())
			return
		}
		env.GatewayTarget = req.GatewayTarget
	}
	if req.Variables != nil {
		env.Variables = req.Variables
	}
	if req.CredentialBindings != nil {
		env.CredentialBindings = req.CredentialBindings
	}
	if req.Revision > 0 {
		env.Revision = req.Revision
	}

	updated, err := s.store.UpdateTestEnvironment(r.Context(), env)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "UPDATE_TEST_ENVIRONMENT", "test_environment", updated.ID, map[string]any{"name": updated.Name})
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteTestEnvironment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	env, err := s.store.GetTestEnvironment(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(env.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	if err := s.store.DeleteTestEnvironment(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "DELETE_TEST_ENVIRONMENT", "test_environment", id, map[string]any{"name": env.Name})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// Test Runs
// ---------------------------------------------------------------------------

type testRunReq struct {
	SuiteID           string   `json:"suite_id"`
	SuiteVersion      int      `json:"suite_version"`
	EnvironmentID     string   `json:"environment_id"`
	TargetRevision    int64    `json:"target_revision"`
	ExecutionMode     string   `json:"execution_mode"`
	BaselineRevision  int64    `json:"baseline_revision"`
	CandidateRevision int64    `json:"candidate_revision"`
	IgnoreJSONPaths   []string `json:"ignore_json_paths"`
}

func (s *Server) createTestRun(w http.ResponseWriter, r *http.Request) {
	var req testRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse request JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(req.SuiteID) == "" {
		writeErr(w, http.StatusBadRequest, "missing_suite_id", "suite_id is required")
		return
	}

	suite, err := s.store.GetTestSuite(r.Context(), req.SuiteID)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(suite.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	var ver store.TestSuiteVersion
	if req.SuiteVersion > 0 {
		ver, err = s.store.GetTestSuiteVersion(r.Context(), req.SuiteID, req.SuiteVersion)
	} else {
		ver, err = s.store.GetLatestTestSuiteVersion(r.Context(), req.SuiteID)
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	mode := req.ExecutionMode
	if mode == "" || mode == "direct" {
		mode = "standard"
	}
	if mode != "standard" && mode != "comparison" && mode != "canary_gate" {
		writeErr(w, http.StatusBadRequest, "invalid_mode", "execution_mode must be 'standard' or 'comparison'")
		return
	}

	immutableInputs := map[string]any{
		"target_revision":    req.TargetRevision,
		"baseline_revision":  req.BaselineRevision,
		"candidate_revision": req.CandidateRevision,
		"ignore_paths":       req.IgnoreJSONPaths,
	}

	verID := ver.ID
	run := store.TestRun{
		TenantID:         suite.TenantID,
		SuiteID:          suite.ID,
		SuiteVersionID:   &verID,
		SuiteContentHash: ver.ContentHash,
		Actor:            s.getActor(r),
		Mode:             mode,
		LifecycleState:   "queued",
		TotalSteps:       len(ver.Definition.Requests),
		ImmutableInputs:  immutableInputs,
		ActualRevision:   req.TargetRevision,
	}
	if req.EnvironmentID != "" {
		env, err := s.store.GetTestEnvironment(r.Context(), req.EnvironmentID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_environment", "environment not found")
			return
		}
		if !sc.allows(env.TenantID) || (suite.TenantID != "" && env.TenantID != suite.TenantID) {
			writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied for specified environment")
			return
		}
		envID := req.EnvironmentID
		run.EnvironmentID = &envID
		versions, err := s.store.TestCredentialVersions(r.Context(), env)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_credentials", err.Error())
			return
		}
		immutableInputs["environment_snapshot"] = map[string]any{
			"id":                  env.ID,
			"name":                env.Name,
			"gateway_target":      env.GatewayTarget,
			"variables":           env.Variables,
			"credential_bindings": env.CredentialBindings,
			"credential_versions": versions,
			"revision":            env.Revision,
		}
	}

	created, err := s.store.CreateTestRun(r.Context(), run)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Enqueue in durable queue
	_, err = s.store.EnqueueTestJob(r.Context(), created.ID, created.TenantID)
	if err != nil {
		slog.Error("Failed to enqueue test job", "run_id", created.ID, "error", err)
	}

	s.audit(r, "TRIGGER_TEST_RUN", "test_run", created.ID, map[string]any{"suite_id": suite.ID, "mode": mode})
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) listTestRuns(w http.ResponseWriter, r *http.Request) {
	sc := scopeFrom(r)
	tenantID := ""
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}
	if qTenant := r.URL.Query().Get("tenant"); qTenant != "" && sc.allows(qTenant) {
		tenantID = qTenant
	}

	suiteID := r.URL.Query().Get("suite_id")
	limit := 50
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if parsed, err := strconv.Atoi(lStr); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}

	runs, err := s.store.ListTestRuns(r.Context(), tenantID, suiteID, limit)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getTestRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.store.GetTestRun(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(run.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	steps, err := s.store.ListTestRunSteps(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"run":   run,
		"steps": steps,
	})
}

func (s *Server) cancelTestRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.store.GetTestRun(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(run.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	if err := s.store.CancelTestRun(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "CANCEL_TEST_RUN", "test_run", id, map[string]any{"suite_id": run.SuiteID})
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "run_id": id})
}

func (s *Server) getTestRunComparison(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.store.GetTestRun(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}

	sc := scopeFrom(r)
	if !sc.allows(run.TenantID) {
		writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
		return
	}

	if run.Mode != "comparison" {
		writeErr(w, http.StatusBadRequest, "not_comparison_run", "run is not in comparison mode")
		return
	}

	steps, _ := s.store.ListTestRunSteps(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":             run.ID,
		"baseline_revision":  run.ActualRevision,
		"candidate_revision": run.ActualCandidateRevision,
		"comparison_summary": run.ComparisonSummary,
		"steps":              steps,
	})
}

// ---------------------------------------------------------------------------
// Promotion Gate Policies
// ---------------------------------------------------------------------------

func (s *Server) getTestGatePolicy(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("api_id")
	sc := scopeFrom(r)
	tenantID := store.DefaultTenantID
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}
	if qTenant := r.URL.Query().Get("tenant"); qTenant != "" && sc.allows(qTenant) {
		tenantID = qTenant
	}

	policy, err := s.store.GetTestGatePolicy(r.Context(), tenantID, apiID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if policy == nil {
		var aPtr *string
		if trimmed := strings.TrimSpace(apiID); trimmed != "" {
			aPtr = &trimmed
		}
		policy = &store.TestGatePolicy{
			TenantID:           tenantID,
			APIID:              aPtr,
			TargetEnvironment:  "canary",
			RequiredSuiteIDs:   []string{},
			FreshnessSeconds:   3600,
			EnforcementEnabled: false,
		}
	}

	var latestEvidence *store.TestGateEvidence
	var rev int64 = 0
	if rRev := r.URL.Query().Get("revision"); rRev != "" {
		if parsed, err := strconv.ParseInt(rRev, 10, 64); err == nil {
			rev = parsed
		}
	}
	if rev > 0 {
		latestEvidence, _ = s.store.GetLatestEligibleEvidence(r.Context(), tenantID, apiID, rev)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"policy":   policy,
		"evidence": latestEvidence,
	})
}

func (s *Server) upsertTestGatePolicy(w http.ResponseWriter, r *http.Request) {
	apiID := r.PathValue("api_id")
	sc := scopeFrom(r)
	tenantID := store.DefaultTenantID
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}

	var p store.TestGatePolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse request JSON: "+err.Error())
		return
	}

	if p.TenantID != "" {
		if !sc.allows(p.TenantID) {
			writeErr(w, http.StatusForbidden, "forbidden", "tenant access denied")
			return
		}
		tenantID = p.TenantID
	}
	p.TenantID = tenantID
	var aPtr *string
	if trimmed := strings.TrimSpace(apiID); trimmed != "" {
		aPtr = &trimmed
	}
	p.APIID = aPtr
	if p.TargetEnvironment == "" {
		p.TargetEnvironment = "canary"
	}
	if p.RequiredSuiteIDs == nil {
		p.RequiredSuiteIDs = []string{}
	}

	saved, err := s.store.UpsertTestGatePolicy(r.Context(), p)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "UPDATE_GATE_POLICY", "gate_policy", apiID, map[string]any{
		"enforce": saved.EnforcementEnabled,
		"suites":  saved.RequiredSuiteIDs,
	})
	writeJSON(w, http.StatusOK, saved)
}

// ---------------------------------------------------------------------------
// OpenAPI / Postman Import
// ---------------------------------------------------------------------------

type importReq struct {
	Format  string `json:"format"` // "openapi" or "postman"
	Content string `json:"content"`
	Name    string `json:"name"`
	APIID   string `json:"api_id"`
}

func (s *Server) importTestSuite(w http.ResponseWriter, r *http.Request) {
	var req importReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_body", "failed to parse JSON: "+err.Error())
		return
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format != "openapi" && format != "postman" {
		writeErr(w, http.StatusBadRequest, "invalid_format", "format must be 'openapi' or 'postman'")
		return
	}

	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeErr(w, http.StatusBadRequest, "empty_content", "content string is required")
		return
	}

	sc := scopeFrom(r)
	tenantID := "default"
	if !sc.All && len(sc.TenantIDs) > 0 {
		tenantID = sc.TenantIDs[0]
	}

	var suiteName = strings.TrimSpace(req.Name)
	var reqs []store.RequestDef

	if format == "postman" {
		var postmanDoc struct {
			Info struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"info"`
			Item []any `json:"item"`
		}
		if err := json.Unmarshal([]byte(content), &postmanDoc); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_postman_json", "failed to parse Postman collection: "+err.Error())
			return
		}
		if suiteName == "" {
			suiteName = postmanDoc.Info.Name
		}
		reqs = parsePostmanItems(postmanDoc.Item)
	} else {
		// OpenAPI format (JSON or YAML)
		var openapiDoc struct {
			Info struct {
				Title string `json:"title"`
			} `json:"info"`
			Paths map[string]map[string]struct {
				Summary     string         `json:"summary"`
				OperationID string         `json:"operationId"`
				Responses   map[string]any `json:"responses"`
			} `json:"paths"`
		}
		err := json.Unmarshal([]byte(content), &openapiDoc)
		if err != nil {
			err = yaml.Unmarshal([]byte(content), &openapiDoc)
		}
		if err != nil || len(openapiDoc.Paths) == 0 {
			writeErr(w, http.StatusBadRequest, "invalid_openapi", "failed to parse OpenAPI document or no paths found")
			return
		}
		if suiteName == "" {
			suiteName = openapiDoc.Info.Title
		}
		idx := 1
		for path, methods := range openapiDoc.Paths {
			for method, op := range methods {
				m := strings.ToUpper(method)
				if m != "GET" && m != "POST" && m != "PUT" && m != "DELETE" && m != "PATCH" && m != "HEAD" && m != "OPTIONS" {
					continue
				}
				reqName := op.Summary
				if reqName == "" {
					reqName = op.OperationID
				}
				if reqName == "" {
					reqName = fmt.Sprintf("%s %s", m, path)
				}
				cleanPath := cleanEndpointPath(path)
				reqs = append(reqs, store.RequestDef{
					ID:     fmt.Sprintf("req-%d", idx),
					Name:   reqName,
					Method: m,
					Path:   cleanPath,
					Assertions: []store.AssertionDef{
						{Type: "status_code", Target: "status_code", Expected: "200"},
					},
				})
				idx++
			}
		}
	}

	if suiteName == "" {
		suiteName = "Imported Test Suite"
	}
	if len(reqs) == 0 {
		writeErr(w, http.StatusBadRequest, "no_steps_imported", "no valid endpoints or requests found in document")
		return
	}

	var apiIDPtr *string
	if trimmed := strings.TrimSpace(req.APIID); trimmed != "" {
		apiIDPtr = &trimmed
	}

	suite := store.TestSuite{
		TenantID:    tenantID,
		Name:        suiteName,
		Description: fmt.Sprintf("Imported from %s on %s", format, time.Now().Format(time.RFC3339)),
		APIID:       apiIDPtr,
		Ownership:   "team",
		Visibility:  "tenant",
		CreatedBy:   s.getActor(r),
	}
	def := store.SuiteDefinition{
		Name:        suiteName,
		Description: suite.Description,
		Requests:    reqs,
	}

	created, ver, err := s.store.CreateTestSuite(r.Context(), suite, def, s.getActor(r))
	if err != nil {
		s.fail(w, err)
		return
	}

	s.audit(r, "IMPORT_TEST_SUITE", "test_suite", created.ID, map[string]any{"format": format, "request_count": len(reqs)})
	writeJSON(w, http.StatusCreated, map[string]any{
		"suite":         created,
		"version":       ver,
		"request_count": len(reqs),
	})
}

func parsePostmanItems(items []any) []store.RequestDef {
	var reqs []store.RequestDef
	idx := 1
	var recurse func(list []any)
	recurse = func(list []any) {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if subItems, hasSubs := m["item"].([]any); hasSubs {
				recurse(subItems)
				continue
			}
			reqObj, hasReq := m["request"].(map[string]any)
			if !hasReq {
				continue
			}

			name, _ := m["name"].(string)
			method, _ := reqObj["method"].(string)
			if method == "" {
				method = "GET"
			}

			rawURL := ""
			if urlStr, ok := reqObj["url"].(string); ok {
				rawURL = urlStr
			} else if urlMap, ok := reqObj["url"].(map[string]any); ok {
				rawURL, _ = urlMap["raw"].(string)
			}

			cleanPath := cleanEndpointPath(rawURL)

			var headers map[string]string
			if hdrList, ok := reqObj["header"].([]any); ok {
				headers = map[string]string{}
				for _, h := range hdrList {
					if hMap, ok := h.(map[string]any); ok {
						k, _ := hMap["key"].(string)
						v, _ := hMap["value"].(string)
						if k != "" {
							headers[k] = v
						}
					}
				}
			}

			bodyStr := ""
			if bodyMap, ok := reqObj["body"].(map[string]any); ok {
				if raw, ok := bodyMap["raw"].(string); ok {
					bodyStr = raw
				}
			}

			if name == "" {
				name = fmt.Sprintf("%s %s", method, cleanPath)
			}

			reqs = append(reqs, store.RequestDef{
				ID:         fmt.Sprintf("req-%d", idx),
				Name:       name,
				Method:     strings.ToUpper(method),
				Path:       cleanPath,
				Headers:    headers,
				Body:       bodyStr,
				Assertions: []store.AssertionDef{{Type: "status_code", Target: "status_code", Expected: "200"}},
			})
			idx++
		}
	}
	recurse(items)
	return reqs
}

func cleanEndpointPath(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		if parsed, err := url.Parse(raw); err == nil {
			raw = parsed.Path
			if parsed.RawQuery != "" {
				raw += "?" + parsed.RawQuery
			}
		}
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return raw
}
