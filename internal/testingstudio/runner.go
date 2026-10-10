package testingstudio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/relayops/apim/internal/store"
)

type RunnerConfig struct {
	WorkerID          string
	PollInterval      time.Duration
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	MaxGlobalRuns     int
	DefaultTimeout    time.Duration
	RunnerToken       string
	TrustedGateways   []string
}

type Runner struct {
	store      *store.Store
	cfg        RunnerConfig
	client     *http.Client
	stopCh     chan struct{}
	wakeCh     chan struct{}
	wg         sync.WaitGroup
	activeMu   sync.Mutex
	activeRuns map[string]context.CancelFunc
}

func NewRunner(s *store.Store, cfg RunnerConfig) *Runner {
	if cfg.WorkerID == "" {
		host, _ := os.Hostname()
		cfg.WorkerID = fmt.Sprintf("worker-%s-%d", host, os.Getpid())
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 60 * time.Second
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.MaxGlobalRuns <= 0 {
		cfg.MaxGlobalRuns = 4
	}
	if cfg.DefaultTimeout <= 0 {
		cfg.DefaultTimeout = 30 * time.Second
	}

	return &Runner{
		store: s,
		cfg:   cfg,
		client: &http.Client{
			Transport: newSafeTransport(cfg.TrustedGateways),
			Timeout:   cfg.DefaultTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // Do not auto-follow redirects initially
			},
		},
		stopCh:     make(chan struct{}),
		wakeCh:     make(chan struct{}, 1),
		activeRuns: make(map[string]context.CancelFunc),
	}
}

// Wake immediately signals the runner loop to check for pending jobs.
func (r *Runner) Wake() {
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

func (r *Runner) Start() {
	r.wg.Add(1)
	go r.loop()
	if r.store != nil && r.store.Pool != nil {
		r.wg.Add(1)
		go r.listenNotifications()
	}
}

func (r *Runner) Stop() {
	close(r.stopCh)
	r.activeMu.Lock()
	for _, cancel := range r.activeRuns {
		cancel()
	}
	r.activeMu.Unlock()
	r.wg.Wait()
}

func (r *Runner) listenNotifications() {
	defer r.wg.Done()
	backoff := time.Second
	for {
		select {
		case <-r.stopCh:
			return
		default:
		}

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-r.stopCh:
				cancel()
			case <-ctx.Done():
			}
		}()

		err := func() error {
			conn, err := r.store.Pool.Acquire(ctx)
			if err != nil {
				return err
			}
			defer conn.Release()

			if _, err := conn.Exec(ctx, "LISTEN relayops_test_jobs"); err != nil {
				return err
			}
			backoff = time.Second
			for {
				_, err := conn.Conn().WaitForNotification(ctx)
				if err != nil {
					return err
				}
				r.Wake()
			}
		}()
		cancel()

		if err != nil {
			select {
			case <-r.stopCh:
				return
			case <-time.After(backoff):
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		}
	}
}

func (r *Runner) loop() {
	defer r.wg.Done()
	fastInterval := r.cfg.PollInterval
	if fastInterval <= 0 {
		fastInterval = 1 * time.Second
	}
	maxIdleInterval := 30 * time.Second
	if maxIdleInterval < fastInterval {
		maxIdleInterval = fastInterval
	}
	currentInterval := fastInterval

	ticker := time.NewTicker(currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-r.wakeCh:
			currentInterval = fastInterval
			ticker.Reset(currentInterval)
			r.pollAndExecute()
		case <-ticker.C:
			claimed := r.pollAndExecute()
			if claimed {
				if currentInterval != fastInterval {
					currentInterval = fastInterval
					ticker.Reset(currentInterval)
				}
			} else {
				if currentInterval < maxIdleInterval {
					currentInterval = currentInterval * 2
					if currentInterval > maxIdleInterval {
						currentInterval = maxIdleInterval
					}
					ticker.Reset(currentInterval)
				}
			}
		}
	}
}

func (r *Runner) pollAndExecute() bool {
	r.activeMu.Lock()
	if len(r.activeRuns) >= r.cfg.MaxGlobalRuns {
		r.activeMu.Unlock()
		return false
	}
	r.activeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	job, err := r.store.ClaimTestJob(ctx, r.cfg.WorkerID, r.cfg.LeaseDuration)
	if err != nil || job == nil {
		return false
	}

	r.wg.Add(1)
	go func(j store.TestJob) {
		defer r.wg.Done()
		r.executeJob(j)
	}(*job)
	return true
}

func (r *Runner) executeJob(job store.TestJob) {
	runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	r.activeMu.Lock()
	r.activeRuns[job.RunID] = cancel
	r.activeMu.Unlock()

	defer func() {
		r.activeMu.Lock()
		delete(r.activeRuns, job.RunID)
		r.activeMu.Unlock()
	}()

	// Background heartbeat
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		hbTicker := time.NewTicker(r.cfg.HeartbeatInterval)
		defer hbTicker.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-hbTicker.C:
				if err := r.store.HeartbeatTestJob(runCtx, job.ID, r.cfg.WorkerID, r.cfg.LeaseDuration); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	run, err := r.store.GetTestRun(runCtx, job.RunID)
	if err != nil {
		_ = r.store.CompleteTestJob(context.Background(), job.ID, "failed")
		return
	}

	if run.LifecycleState == "cancelled" {
		_ = r.store.CompleteTestJob(context.Background(), job.ID, "cancelled")
		return
	}

	// Load Version, Suite & Environment
	suite, _ := r.store.GetTestSuite(runCtx, run.SuiteID)

	var def store.SuiteDefinition
	if run.SuiteVersionID != nil {
		row := r.store.Pool.QueryRow(runCtx, `SELECT definition FROM test_suite_versions WHERE id=$1`, *run.SuiteVersionID)
		var defJSON []byte
		if err := row.Scan(&defJSON); err == nil {
			_ = json.Unmarshal(defJSON, &def)
		}
	}
	if len(def.Requests) == 0 {
		latest, err := r.store.GetLatestTestSuiteVersion(runCtx, run.SuiteID)
		if err == nil {
			def = latest.Definition
		}
	}
	if raw, ok := run.ImmutableInputs["ignore_paths"]; ok && raw != nil {
		if encoded, err := json.Marshal(raw); err == nil {
			var paths []string
			if json.Unmarshal(encoded, &paths) == nil && len(paths) > 0 {
				def.IgnorePaths = paths
			}
		}
	}

	var env store.TestEnvironment
	if run.ImmutableInputs != nil {
		if snapRaw, ok := run.ImmutableInputs["environment_snapshot"].(map[string]any); ok {
			env.ID, _ = snapRaw["id"].(string)
			env.Name, _ = snapRaw["name"].(string)
			env.GatewayTarget, _ = snapRaw["gateway_target"].(string)
			if revFloat, ok := snapRaw["revision"].(float64); ok {
				env.Revision = int(revFloat)
			}
			if varsMap, ok := snapRaw["variables"].(map[string]any); ok {
				env.Variables = make(map[string]string)
				for k, v := range varsMap {
					env.Variables[k] = fmt.Sprintf("%v", v)
				}
			}
			if credMap, ok := snapRaw["credential_bindings"].(map[string]any); ok {
				env.CredentialBindings = make(map[string]string)
				for k, v := range credMap {
					env.CredentialBindings[k] = fmt.Sprintf("%v", v)
				}
			}
		}
	}
	if env.GatewayTarget == "" && run.EnvironmentID != nil {
		env, _ = r.store.GetTestEnvironment(runCtx, *run.EnvironmentID)
	}

	// Mark running
	_ = r.store.UpdateTestRunProgress(runCtx, run.ID, "running", 0, 0, 0, "", 0, 0, nil)

	// Runtime variables state
	vars := make(map[string]string)
	for k, v := range env.Variables {
		vars[k] = v
	}
	for _, v := range def.Variables {
		if _, exists := vars[v.Key]; !exists {
			vars[v.Key] = v.DefaultValue
		}
	}

	gatewayTarget := env.GatewayTarget
	if gatewayTarget == "" {
		gatewayTarget = "http://127.0.0.1:8080"
	}
	gatewayTarget = strings.TrimRight(gatewayTarget, "/")

	// Extract pinned revisions from inputs
	var baselineRevPin int64
	var candidateRevPin int64
	if run.ImmutableInputs != nil {
		if b, ok := run.ImmutableInputs["baseline_revision"].(float64); ok {
			baselineRevPin = int64(b)
		} else if b, ok := run.ImmutableInputs["baseline_revision"].(int64); ok {
			baselineRevPin = b
		}
		if c, ok := run.ImmutableInputs["candidate_revision"].(float64); ok {
			candidateRevPin = int64(c)
		} else if c, ok := run.ImmutableInputs["candidate_revision"].(int64); ok {
			candidateRevPin = c
		}
		if baselineRevPin == 0 {
			if t, ok := run.ImmutableInputs["target_revision"].(float64); ok {
				baselineRevPin = int64(t)
			} else if t, ok := run.ImmutableInputs["target_revision"].(int64); ok {
				baselineRevPin = t
			}
		}
	}
	if baselineRevPin == 0 && run.ActualRevision > 0 {
		baselineRevPin = run.ActualRevision
	}

	total := len(def.Requests)
	passed := 0
	failed := 0
	skipped := 0
	var observedRev int64
	var observedCandRev int64
	var comparisons []StepComparison

	isComparison := run.Mode == "comparison" || run.Mode == "canary_gate"

	for idx, reqDef := range def.Requests {
		select {
		case <-runCtx.Done():
			_ = r.store.UpdateTestRunProgress(context.Background(), run.ID, "interrupted", passed, failed, total-(passed+failed), "run deadline exceeded or worker cancelled", observedRev, observedCandRev, nil)
			_ = r.store.CompleteTestJob(context.Background(), job.ID, "failed")
			return
		default:
		}

		// Execute baseline request with explicit cohort identity
		baseCohort := "baseline"
		if !isComparison {
			baseCohort = "standard"
		}
		stepRes, stepObservedRev, err := r.executeRequest(runCtx, run.ID, idx+1, baseCohort, reqDef, gatewayTarget, vars, baselineRevPin, false)
		if err != nil {
			failed++
			continue
		}
		if stepObservedRev > observedRev {
			observedRev = stepObservedRev
		}

		var candStep store.TestRunStep
		var candErr error
		if isComparison {
			// Execute candidate request targeting candidate revision with candidate cohort identity
			var candObservedRev int64
			candStep, candObservedRev, candErr = r.executeRequest(runCtx, run.ID, idx+1, "candidate", reqDef, gatewayTarget, vars, candidateRevPin, true)
			if candErr == nil {
				if candObservedRev > observedCandRev {
					observedCandRev = candObservedRev
				}
				comp := CompareStepsWithPolicy(stepRes, candStep, def.IgnorePaths, def.Comparison)
				comparisons = append(comparisons, comp)
			}
		}

		allPassed := true
		for _, a := range stepRes.AssertionResults {
			if !a.Passed {
				allPassed = false
				break
			}
		}
		if isComparison {
			if candErr != nil {
				allPassed = false
			} else {
				for _, a := range candStep.AssertionResults {
					if !a.Passed {
						allPassed = false
						break
					}
				}
				if len(comparisons) > 0 && comparisons[len(comparisons)-1].BehavioralDiff {
					allPassed = false
				}
			}
		}

		if allPassed {
			passed++
		} else {
			failed++
		}
	}

	finalState := "completed"
	if failed > 0 {
		finalState = "failed"
	}

	var compSummary map[string]any
	if len(comparisons) > 0 {
		diffCount := 0
		for _, c := range comparisons {
			if c.BehavioralDiff {
				diffCount++
			}
		}
		compSummary = map[string]any{
			"engine_version":     store.ComparisonEngineVersion,
			"steps_compared":     len(comparisons),
			"behavioral_diffs":   diffCount,
			"baseline_revision":  observedRev,
			"candidate_revision": observedCandRev,
			"comparisons":        comparisons,
		}
	}

	if err := r.store.UpdateTestRunProgress(context.Background(), run.ID, finalState, passed, failed, skipped, "", observedRev, observedCandRev, compSummary); err != nil {
		return
	}
	_ = r.store.CompleteTestJob(context.Background(), job.ID, "done")

	// Production promotion gate evidence evaluation & recording
	if suite.APIID != nil && *suite.APIID != "" {
		policy, polErr := r.store.GetTestGatePolicy(context.Background(), run.TenantID, *suite.APIID)
		if polErr == nil && policy != nil {
			targetRev := observedRev
			if observedCandRev > 0 {
				targetRev = observedCandRev
			} else if candidateRevPin > 0 {
				targetRev = candidateRevPin
			} else if run.ActualRevision > 0 {
				targetRev = run.ActualRevision
			}

			now := time.Now()
			completedRun := run
			completedRun.LifecycleState = finalState
			completedRun.PassedSteps = passed
			completedRun.FailedSteps = failed
			completedRun.SkippedSteps = skipped
			completedRun.CompletedAt = &now
			completedRun.ActualRevision = targetRev
			completedRun.ActualCandidateRevision = observedCandRev

			eval := EvaluateGateEvidence(completedRun, policy, targetRev)
			evidence := store.TestGateEvidence{
				TenantID:          run.TenantID,
				APIID:             suite.APIID,
				Revision:          targetRev,
				RunID:             &run.ID,
				TargetFingerprint: run.SuiteContentHash,
				Eligible:          eval.Eligible,
				Reasons:           eval.Reasons,
			}
			_ = r.store.RecordTestGateEvidence(context.Background(), evidence)
		}
	}
}

func (r *Runner) executeRequest(ctx context.Context, runID string, stepIndex int, cohort string, reqDef store.RequestDef, gatewayTarget string, vars map[string]string, targetRevisionPin int64, isCanaryCohort bool) (store.TestRunStep, int64, error) {
	// Interpolate path, body, headers, query params
	method := strings.ToUpper(strings.TrimSpace(reqDef.Method))
	resolvedPath := Interpolate(reqDef.Path, vars)
	resolvedBody := Interpolate(reqDef.Body, vars)

	if !strings.HasPrefix(resolvedPath, "/") {
		resolvedPath = "/" + resolvedPath
	}
	targetURL := gatewayTarget + resolvedPath

	if len(reqDef.QueryParams) > 0 {
		var qParts []string
		for k, v := range reqDef.QueryParams {
			qParts = append(qParts, fmt.Sprintf("%s=%s", k, Interpolate(v, vars)))
		}
		if strings.Contains(targetURL, "?") {
			targetURL += "&" + strings.Join(qParts, "&")
		} else {
			targetURL += "?" + strings.Join(qParts, "&")
		}
	}

	var bodyReader io.Reader
	if len(resolvedBody) > 0 {
		bodyReader = bytes.NewReader([]byte(resolvedBody))
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return store.TestRunStep{Cohort: cohort}, 0, err
	}

	// Headers
	reqHeaders := make(map[string]string)
	for k, v := range reqDef.Headers {
		val := Interpolate(v, vars)
		httpReq.Header.Set(k, val)
		reqHeaders[k] = val
	}
	if httpReq.Header.Get("Content-Type") == "" && len(resolvedBody) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
		reqHeaders["Content-Type"] = "application/json"
	}

	// Auth interpolation
	if reqDef.Auth.Type == "api_key" && reqDef.Auth.TokenValue != "" {
		keyVal := Interpolate(reqDef.Auth.TokenValue, vars)
		httpReq.Header.Set("X-API-Key", keyVal)
		reqHeaders["X-API-Key"] = keyVal
	} else if (reqDef.Auth.Type == "bearer_token" || reqDef.Auth.Type == "jwt") && reqDef.Auth.TokenValue != "" {
		tokVal := Interpolate(reqDef.Auth.TokenValue, vars)
		httpReq.Header.Set("Authorization", "Bearer "+tokVal)
		reqHeaders["Authorization"] = "Bearer " + tokVal
	}

	// Internal runner authentication and cohort/revision selection:
	// ONLY send runner secret and internal cohort headers if the destination is a trusted gateway!
	if IsTrustedGateway(targetURL, r.cfg.TrustedGateways...) {
		if r.cfg.RunnerToken != "" {
			httpReq.Header.Set("X-RelayOps-Runner-Token", r.cfg.RunnerToken)
		}
		if targetRevisionPin > 0 {
			httpReq.Header.Set("X-RelayOps-Target-Revision", strconv.FormatInt(targetRevisionPin, 10))
		}
		if isCanaryCohort {
			httpReq.Header.Set("X-RelayOps-Target-Cohort", "canary")
		}
	}

	start := time.Now()
	resp, err := r.client.Do(httpReq)
	durationMS := float64(time.Since(start).Microseconds()) / 1000.0

	var statusCode int
	var respBytes []byte
	var respHeaders http.Header
	var observedRev int64
	var policy, reason string

	if err == nil {
		statusCode = resp.StatusCode
		respHeaders = resp.Header

		if revStr := resp.Header.Get("X-RelayOps-Revision"); revStr != "" {
			clean := strings.TrimPrefix(revStr, "rev_")
			observedRev, _ = strconv.ParseInt(clean, 10, 64)
		}
		policy = resp.Header.Get("X-RelayOps-Decision-Policy")
		reason = resp.Header.Get("X-RelayOps-Decision-Reason")

		defer resp.Body.Close()
		respBytes, _ = io.ReadAll(io.LimitReader(resp.Body, MaxResponseBodyCap))
	} else {
		statusCode = 0
		respHeaders = http.Header{}
		reason = fmt.Sprintf("network_error: %v", err)
	}

	// Run assertions
	assertionResults := make([]store.AssertionRes, 0, len(reqDef.Assertions)+1)
	if err != nil {
		assertionResults = append(assertionResults, store.AssertionRes{Type: "transport", Passed: false, Error: "request could not be completed"})
	}
	for _, a := range reqDef.Assertions {
		aRes := EvaluateAssertion(a, statusCode, respHeaders, respBytes, durationMS)
		assertionResults = append(assertionResults, aRes)
	}

	// Gateway revision pin validation: fails if header is missing, malformed, or mismatched
	if err == nil && targetRevisionPin > 0 {
		if observedRev == 0 {
			assertionResults = append(assertionResults, store.AssertionRes{
				Type:     "revision_pin",
				Target:   "X-RelayOps-Revision",
				Expected: fmt.Sprintf("rev_%d", targetRevisionPin),
				Actual:   "missing",
				Passed:   false,
				Error:    fmt.Sprintf("response is missing required X-RelayOps-Revision header (expected rev_%d)", targetRevisionPin),
			})
		} else if observedRev != targetRevisionPin {
			assertionResults = append(assertionResults, store.AssertionRes{
				Type:     "revision_pin",
				Target:   "X-RelayOps-Revision",
				Expected: fmt.Sprintf("rev_%d", targetRevisionPin),
				Actual:   fmt.Sprintf("rev_%d", observedRev),
				Passed:   false,
				Error:    fmt.Sprintf("gateway served revision rev_%d instead of requested revision rev_%d", observedRev, targetRevisionPin),
			})
		}
	}

	// Run variable extracts
	for _, ext := range reqDef.Extracts {
		if ext.Source == "json_path" {
			if val, err := extractJSONPointer(respBytes, ext.Target); err == nil && val != nil {
				vars[ext.VarName] = fmt.Sprintf("%v", val)
			}
		} else if ext.Source == "header" {
			if hVal := respHeaders.Get(ext.Target); hVal != "" {
				vars[ext.VarName] = hVal
			}
		}
	}

	// Record step
	stepRecord := store.TestRunStep{
		RunID:            runID,
		StepIndex:        stepIndex,
		Cohort:           cohort,
		RequestName:      reqDef.Name,
		Method:           method,
		URL:              targetURL,
		TargetRevision:   targetRevisionPin,
		ObservedRevision: observedRev,
		DurationMS:       durationMS,
		StatusCode:       statusCode,
		AssertionResults: assertionResults,
		DecisionPolicy:   policy,
		DecisionReason:   reason,
		RedactedRequest:  SanitizePayloadMap(reqHeaders, []byte(resolvedBody)),
		RedactedResponse: SanitizePayloadMap(RedactHeaders(respHeaders), respBytes),
	}
	stepRecord.RedactedResponse["truncated"] = len(respBytes) >= MaxRedactedPreview

	recorded, saveErr := r.store.RecordTestRunStep(ctx, stepRecord)
	if saveErr != nil {
		return stepRecord, observedRev, saveErr
	}

	return recorded, observedRev, nil
}

// Interpolate replaces {{variable_name}} tokens with mapped variable values.
func Interpolate(template string, vars map[string]string) string {
	if len(vars) == 0 || !strings.Contains(template, "{{") {
		return template
	}

	res := template
	for k, v := range vars {
		token := "{{" + k + "}}"
		res = strings.ReplaceAll(res, token, v)
	}
	return res
}
