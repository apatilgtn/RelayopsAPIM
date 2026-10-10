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
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/relayops/apim/internal/ai"
	"github.com/relayops/apim/internal/store"
)

// AI evaluation runs: the control plane sends each suite case through the
// gateway (so authentication, budgets, metering and logging all apply) to the
// release manifest's model, scores the responses with ai.EvaluateCandidate,
// and stores the results with a digest over them. Model calls cost money, so
// runs only happen on request and are capped in size.

const (
	maxEvalCases    = 50
	evalCaseTimeout = 60 * time.Second
	evalRunTimeout  = 15 * time.Minute
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (s *Server) createAIEvalSuite(w http.ResponseWriter, r *http.Request) {
	tenant, ok := scopeFrom(r).writeTenant(w)
	if !ok {
		return
	}
	var in store.AIEvalSuite
	if !decode(w, r, &in) {
		return
	}
	in.TenantID = tenant
	if strings.TrimSpace(in.Name) == "" {
		writeErr(w, http.StatusBadRequest, "validation_failed", "name is required")
		return
	}
	if _, err := parseEvalCases(in.TestCases); err != nil {
		writeErr(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	suite, err := s.store.CreateAIEvalSuite(r.Context(), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, tenant, "ai_eval_suite:create", "ai_eval_suite", suite.ID, map[string]any{"name": suite.Name, "cases": len(suite.TestCases)})
	writeJSON(w, http.StatusCreated, suite)
}

func (s *Server) listAIEvalSuites(w http.ResponseWriter, r *http.Request) {
	suites, err := s.store.ListAIEvalSuites(r.Context(), scopeFrom(r).filter())
	s.respond(w, http.StatusOK, suites, err)
}

// parseEvalCases validates suite test cases: 1-50 cases, each with an id and a prompt.
func parseEvalCases(raw []any) ([]ai.EvalTestCase, error) {
	b, _ := json.Marshal(raw)
	var cases []ai.EvalTestCase
	if err := json.Unmarshal(b, &cases); err != nil {
		return nil, fmt.Errorf("test_cases: %w", err)
	}
	if len(cases) == 0 || len(cases) > maxEvalCases {
		return nil, fmt.Errorf("test_cases must have between 1 and %d cases", maxEvalCases)
	}
	seen := map[string]bool{}
	for i, c := range cases {
		if c.ID == "" || strings.TrimSpace(c.Prompt) == "" {
			return nil, fmt.Errorf("test_cases[%d] needs an id and a prompt", i)
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("test_cases[%d] id %q is duplicated", i, c.ID)
		}
		seen[c.ID] = true
	}
	return cases, nil
}

type evalTarget struct {
	suite      store.AIEvalSuite
	manifest   store.AIReleaseManifest
	cases      []ai.EvalTestCase
	api        store.API
	deployment store.AIModelDeployment
}

func (s *Server) createAIEvalRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SuiteID     string  `json:"suite_id"`
		ManifestID  string  `json:"manifest_id"`
		APIKey      string  `json:"api_key"` // used for this run's gateway calls only; never stored
		MinPassRate float64 `json:"min_pass_rate"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.MinPassRate < 0 || in.MinPassRate > 1 {
		writeErr(w, http.StatusBadRequest, "validation_failed", "min_pass_rate must be between 0 and 1")
		return
	}
	if in.MinPassRate == 0 {
		in.MinPassRate = 0.9
	}
	t, status, msg := s.resolveEvalTarget(r, in.SuiteID, in.ManifestID)
	if status != 0 {
		writeErr(w, status, "invalid_eval_run", msg)
		return
	}
	run, err := s.store.CreateAIEvalRun(r.Context(), store.AIEvalRun{TenantID: t.manifest.TenantID, SuiteID: t.suite.ID,
		ManifestID: t.manifest.ID, MinPassRate: in.MinPassRate, CreatedBy: s.getActor(r)})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.auditTenant(r, t.manifest.TenantID, "ai_eval_run:start", "ai_eval_run", run.ID, map[string]any{
		"suite_id": t.suite.ID, "manifest_id": t.manifest.ID, "cases": len(t.cases), "model": t.deployment.ModelName})
	go s.executeEvalRun(run, t, in.APIKey)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) listAIEvalRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListAIEvalRuns(r.Context(), scopeFrom(r).filter(), 50)
	s.respond(w, http.StatusOK, runs, err)
}

func (s *Server) getAIEvalRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetAIEvalRun(r.Context(), r.PathValue("id"))
	if err != nil || !scopeFrom(r).allows(run.TenantID) {
		writeErr(w, http.StatusNotFound, "not_found", "evaluation run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// resolveEvalTarget loads and cross-checks the suite, manifest, service, API
// and model deployment. It returns an HTTP status and message on failure.
func (s *Server) resolveEvalTarget(r *http.Request, suiteID, manifestID string) (*evalTarget, int, string) {
	ctx := r.Context()
	sc := scopeFrom(r)
	if !uuidPattern.MatchString(suiteID) || !uuidPattern.MatchString(manifestID) {
		return nil, http.StatusBadRequest, "suite_id and manifest_id are required"
	}
	suite, err := s.store.GetAIEvalSuite(ctx, suiteID)
	if err != nil || !sc.allows(suite.TenantID) {
		return nil, http.StatusNotFound, "evaluation suite not found"
	}
	manifest, err := s.store.GetAIReleaseManifest(ctx, manifestID)
	if err != nil || !sc.allows(manifest.TenantID) || manifest.TenantID != suite.TenantID {
		return nil, http.StatusNotFound, "release manifest not found"
	}
	if suite.ServiceID != nil && *suite.ServiceID != manifest.ServiceID {
		return nil, http.StatusBadRequest, "the suite belongs to a different AI service than the manifest"
	}
	cases, err := parseEvalCases(suite.TestCases)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	services, err := s.store.ListAIServices(ctx, manifest.TenantID)
	if err != nil {
		return nil, http.StatusInternalServerError, err.Error()
	}
	var svc *store.AIService
	for i := range services {
		if services[i].ID == manifest.ServiceID {
			svc = &services[i]
		}
	}
	if svc == nil || svc.APIID == nil {
		return nil, http.StatusBadRequest, "the manifest's AI service is not attached to a gateway API"
	}
	api, err := s.store.GetAPI(ctx, *svc.APIID)
	if err != nil || !api.IsAI || !api.Enabled {
		return nil, http.StatusBadRequest, "the AI service's gateway API must exist, be enabled and be an AI API"
	}
	deps, err := s.store.ListAIModelDeployments(ctx, manifest.TenantID)
	if err != nil {
		return nil, http.StatusInternalServerError, err.Error()
	}
	t := &evalTarget{suite: suite, manifest: manifest, cases: cases, api: api}
	found := false
	for _, d := range deps {
		if d.ID == manifest.ModelDeploymentID {
			t.deployment, found = d, true
		}
	}
	if !found {
		return nil, http.StatusBadRequest, "the manifest's model deployment was not found"
	}
	return t, 0, ""
}

func (s *Server) executeEvalRun(run store.AIEvalRun, t *evalTarget, apiKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), evalRunTimeout)
	defer cancel()
	endpoint := "/chat/completions"
	if p, ok := t.suite.Rubric["endpoint_path"].(string); ok && strings.HasPrefix(p, "/") {
		endpoint = p
	}
	maxTokens := 512
	if v, ok := t.suite.Rubric["max_tokens"].(float64); ok && v > 0 && v <= 8192 {
		maxTokens = int(v)
	}
	url := strings.TrimRight(s.gatewayURL, "/") + strings.TrimRight(t.api.BasePath, "/") + endpoint
	client := &http.Client{Timeout: evalCaseTimeout}

	trials := make([]ai.EvalTrialResult, 0, len(t.cases))
	for _, c := range t.cases {
		if ctx.Err() != nil {
			trials = append(trials, ai.EvalTrialResult{TestCaseID: c.ID, Failure: "evaluation run timed out"})
			continue
		}
		trials = append(trials, s.runEvalCase(ctx, client, url, apiKey, run.ID, t, c, maxTokens))
	}
	summary := ai.EvaluateCandidate(t.cases, trials, run.MinPassRate)

	// Digest over what was actually evaluated: model, prompt, cases and every response.
	promptSum := sha256.Sum256([]byte(t.manifest.SystemPrompt))
	evidence := map[string]any{
		"run_id": run.ID, "suite_id": t.suite.ID, "manifest_id": t.manifest.ID, "tenant_id": t.manifest.TenantID,
		"model": t.deployment.ModelName, "system_prompt_sha256": hex.EncodeToString(promptSum[:]),
		"cases": t.cases, "results": summary.Results, "pass_rate": summary.PassRate,
		"safety_violations": summary.SafetyViolations, "qualified": summary.Qualified, "min_pass_rate": run.MinPassRate,
	}
	raw, _ := json.Marshal(evidence)
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	summary.EvidenceDigest = digest
	var out map[string]any
	b, _ := json.Marshal(summary)
	_ = json.Unmarshal(b, &out)
	out["model"] = t.deployment.ModelName
	if err := s.store.FinishAIEvalRun(context.Background(), run.ID, "completed", out, digest, ""); err != nil {
		slog.Error("record AI evaluation run", "run", run.ID, "err", err)
	}
}

func (s *Server) runEvalCase(ctx context.Context, client *http.Client, url, apiKey, runID string, t *evalTarget, c ai.EvalTestCase, maxTokens int) ai.EvalTrialResult {
	trial := ai.EvalTrialResult{TestCaseID: c.ID}
	messages := []map[string]string{}
	if t.manifest.SystemPrompt != "" {
		messages = append(messages, map[string]string{"role": "system", "content": t.manifest.SystemPrompt})
	}
	messages = append(messages, map[string]string{"role": "user", "content": c.Prompt})
	body, _ := json.Marshal(map[string]any{"model": t.deployment.ModelName, "messages": messages,
		"max_tokens": maxTokens, "temperature": 0, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		trial.Failure = err.Error()
		return trial
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-RelayOps-Eval-Run", runID)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	started := time.Now()
	resp, err := client.Do(req)
	trial.LatencyMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		trial.Failure = "model call failed: " + err.Error()
		return trial
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		trial.Failure = fmt.Sprintf("model call returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
		return trial
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		trial.Failure = "unreadable model response: " + truncate(string(raw), 200)
		return trial
	}
	trial.Response = out.Choices[0].Message.Content
	d := t.deployment
	trial.CostCents = int64(math.Ceil((float64(out.Usage.PromptTokens)*d.InputPricePerMillion + float64(out.Usage.CompletionTokens)*d.OutputPricePerMillion) / 1e6 * 100))
	return trial
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// verifiedEvalRun returns the server-executed evaluation run evalRunID for
// manifest, or nil when evalRunID is not a server run (caller-attested). A
// server run that does not belong to the manifest, is unfinished or did not
// qualify is an error.
func (s *Server) verifiedEvalRun(ctx context.Context, manifest store.AIReleaseManifest, evalRunID string) (*store.AIEvalRun, error) {
	if !uuidPattern.MatchString(evalRunID) {
		return nil, nil
	}
	run, err := s.store.GetAIEvalRun(ctx, evalRunID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case run.ManifestID != manifest.ID || run.TenantID != manifest.TenantID:
		return nil, errors.New("evaluation run was executed for a different release manifest")
	case run.Status != "completed":
		return nil, fmt.Errorf("evaluation run is %s, not completed", run.Status)
	case run.Summary["qualified"] != true:
		return nil, errors.New("evaluation run did not qualify the release (pass rate or safety probes)")
	}
	return &run, nil
}
