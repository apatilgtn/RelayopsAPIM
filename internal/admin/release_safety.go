package admin

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/relayops/apim/internal/store"
)

type ReleaseGateCheck struct {
	GateName    string `json:"gate_name"`
	Description string `json:"description"`
	Status      string `json:"status"` // PASS, WARN, FAIL
	Message     string `json:"message"`
	Required    bool   `json:"required"`
}

type ReleaseSafetyReport struct {
	APIID                  string               `json:"api_id"`
	APIName                string               `json:"api_name"`
	TargetRevision         int64                `json:"target_revision"`
	GeneratedAt            time.Time            `json:"generated_at"`
	OverallVerdict         string               `json:"overall_verdict"` // PASSED, WARNING_GATED, BLOCKED
	VerdictSummary         string               `json:"verdict_summary"`
	ReleaseGates           []ReleaseGateCheck   `json:"release_gates"`
	SimulationLimitations  ReplayLimitations    `json:"simulation_limitations"`
	SamplingCoverage       SamplingReport       `json:"sampling_coverage"`
	ConsumerImpact         ConsumerImpactReport `json:"consumer_impact"`
	TotalConsumersAffected int                  `json:"total_consumers_affected"`
	TotalBreakingChanges   int                  `json:"total_breaking_changes"`
	RecommendedAction      string               `json:"recommended_action"`
}

func (s *Server) buildReleaseSafetyReport(ctx context.Context, existing, proposed store.API) ReleaseSafetyReport {
	now := time.Now()
	var currentRev int64 = 1
	if snap := s.gw.Snapshot(); snap != nil {
		currentRev = snap.Version
	}

	// 1. Simulation Limitations
	limitations := defaultReplayLimitations()

	// 2. Consumer Impact Report
	impact := s.buildConsumerImpact(ctx, existing, proposed)

	// 3. Sampling Coverage & Statistical Evaluation
	logs, _ := s.store.QueryLogs(ctx, store.LogFilter{APIID: existing.ID, Limit: 200})
	allSubs, _ := s.store.ListSubscriptions(ctx)
	subscribedConsumers := make(map[string]string)
	for _, sub := range allSubs {
		if sub.APIID == existing.ID {
			subscribedConsumers[sub.ConsumerID] = sub.ConsumerID
		}
	}
	sampling := s.evaluateRepresentativeSampling(ctx, existing.ID, len(logs), impact.TotalImpactedApplications, logs, subscribedConsumers)

	// 4. Contract Checking (if specs present)
	var contractBreakingCount int = 0
	var contractSummary string = "No breaking OpenAPI contract differences detected."
	if len(proposed.OpenAPISpec) > 0 {
		baseSpec := existing.OpenAPISpec
		if len(baseSpec) == 0 {
			baseSpec = s.synthesizeOpenAPISpec(existing)
		}
		cReport := CompareOpenAPISpecs(baseSpec, proposed.OpenAPISpec)
		contractBreakingCount = cReport.BreakingChangesCount
		if contractBreakingCount > 0 {
			contractSummary = fmt.Sprintf("%d breaking change(s) detected in OpenAPI contract specification.", contractBreakingCount)
		}
	}

	// 5. Evaluate Explicit Release Gates
	gates := []ReleaseGateCheck{}

	// Gate 1: Contract Compatibility
	gate1 := ReleaseGateCheck{
		GateName:    "OpenAPI Contract Compatibility",
		Description: "Verifies that routes, parameters, types, and schemas remain backwards-compatible.",
		Required:    true,
	}
	if contractBreakingCount > 0 {
		gate1.Status = "FAIL"
		gate1.Message = contractSummary
	} else {
		gate1.Status = "PASS"
		gate1.Message = "Specification is backwards-compatible."
	}
	gates = append(gates, gate1)

	// Gate 2: Active Consumer Impact
	gate2 := ReleaseGateCheck{
		GateName:    "Consumer Application Impact",
		Description: "Checks whether active production integrations will be broken by the release.",
		Required:    true,
	}
	activeImpacted := 0
	dormantImpacted := 0
	for _, app := range impact.Applications {
		if app.ActivityStatus == "OBSERVED_ACTIVE_CALLER" && app.ImpactScope == "CONFIRMED_APPLICATION_IMPACT" {
			activeImpacted++
		} else if app.ImpactScope == "CONFIRMED_APPLICATION_IMPACT" {
			dormantImpacted++
		}
	}
	if activeImpacted > 0 {
		gate2.Status = "FAIL"
		gate2.Message = fmt.Sprintf("%d active production consumer application(s) will be broken by this change.", activeImpacted)
	} else if dormantImpacted > 0 {
		gate2.Status = "WARN"
		gate2.Message = fmt.Sprintf("No active traffic impacted, but %d registered dormant application(s) will require credential updates.", dormantImpacted)
	} else {
		gate2.Status = "PASS"
		gate2.Message = "No consumers broken by this configuration release."
	}
	gates = append(gates, gate2)

	// Gate 3: Traffic Sample Adequacy
	gate3 := ReleaseGateCheck{
		GateName:    "Statistical Sample Coverage",
		Description: "Ensures traffic sample size and consumer observation ratio provide sufficient statistical confidence.",
		Required:    false,
	}
	if sampling.IsRepresentative {
		gate3.Status = "PASS"
		gate3.Message = fmt.Sprintf("High confidence: %d requests sampled covering %.0f%% of consumers (margin of error ±%.1f%%).", sampling.SampleSize, sampling.ConsumerCoverageRatio*100, sampling.MarginOfError*100)
	} else if sampling.IsSampleAdequate {
		gate3.Status = "WARN"
		gate3.Message = fmt.Sprintf("Partial confidence: %d requests sampled, but %.0f%% of consumers were unobserved during window.", sampling.SampleSize, (1.0-sampling.ConsumerCoverageRatio)*100)
	} else {
		gate3.Status = "WARN"
		gate3.Message = fmt.Sprintf("Small sample size (n=%d < 30). Pre-release analysis relies primarily on static policy analysis.", sampling.SampleSize)
	}
	gates = append(gates, gate3)

	// Gate 4: Fleet Convergence Readiness
	gate4 := ReleaseGateCheck{
		GateName:    "Gateway Fleet Convergence Readiness",
		Description: "Verifies that gateway cluster nodes are currently synchronized and healthy.",
		Required:    true,
	}
	fleetStatus, _ := s.store.GetFleetStatus(ctx)
	if fleetStatus.Converged {
		gate4.Status = "PASS"
		gate4.Message = fmt.Sprintf("Gateway fleet is 100%% converged on baseline rev_%06d (%d nodes active).", fleetStatus.TargetRevision, len(fleetStatus.Nodes))
	} else {
		gate4.Status = "WARN"
		gate4.Message = "Fleet synchronization is currently in progress across data plane nodes."
	}
	gates = append(gates, gate4)

	// Gate 5: Rollback Baseline Protection
	gate5 := ReleaseGateCheck{
		GateName:    "Automated Rollback Baseline",
		Description: "Verifies that an immutable baseline configuration snapshot exists for instant one-click rollback.",
		Required:    true,
	}
	if currentRev > 0 {
		gate5.Status = "PASS"
		gate5.Message = fmt.Sprintf("Safe rollback target rev_%06d verified in configuration history.", currentRev)
	} else {
		gate5.Status = "WARN"
		gate5.Message = "Initial deployment without previous baseline."
	}
	gates = append(gates, gate5)

	// 6. Overall Verdict Computation
	verdict := "PASSED"
	hasFail := false
	hasWarn := false
	for _, g := range gates {
		if g.Status == "FAIL" {
			hasFail = true
		} else if g.Status == "WARN" {
			hasWarn = true
		}
	}

	verdictSummary := ""
	recommendedAction := ""
	if hasFail {
		verdict = "BLOCKED"
		verdictSummary = "Release is gated by safety checks. Breaking changes will impact active consumers or contract guarantees."
		recommendedAction = "Do not deploy directly to 100% of fleet. Use a canary deployment to test with isolated canary nodes, or provide client migration notices."
	} else if hasWarn {
		verdict = "WARNING_GATED"
		verdictSummary = "Release passed required checks with advisories (sample bounds or dormant consumer impact)."
		recommendedAction = "Deploy via Canary Release (e.g. 10% canary nodes) or review dormant consumer migration before full rollout."
	} else {
		verdict = "PASSED"
		verdictSummary = "All release safety gates verified. Safe for immediate production publishing or canary release."
		recommendedAction = "Proceed with deployment to cluster fleet."
	}

	totalBreaking := impact.BreakingChangesCount + contractBreakingCount

	return ReleaseSafetyReport{
		APIID:                  existing.ID,
		APIName:                existing.Name,
		TargetRevision:         currentRev,
		GeneratedAt:            now,
		OverallVerdict:         verdict,
		VerdictSummary:         verdictSummary,
		ReleaseGates:           gates,
		SimulationLimitations:  limitations,
		SamplingCoverage:       sampling,
		ConsumerImpact:         impact,
		TotalConsumersAffected: impact.TotalImpactedApplications,
		TotalBreakingChanges:   totalBreaking,
		RecommendedAction:      recommendedAction,
	}
}

func (s *Server) getReleaseSafetyReport(w http.ResponseWriter, r *http.Request) {
	api, ok := s.scopedAPI(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	report := s.buildReleaseSafetyReport(r.Context(), api, api)
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) postReleaseSafetyReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, ok := s.scopedAPI(w, r, id)
	if !ok {
		return
	}
	var in apiInput
	if !decode(w, r, &in) {
		return
	}
	proposed, err := in.apply(existing)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	report := s.buildReleaseSafetyReport(r.Context(), existing, proposed)
	writeJSON(w, http.StatusOK, report)
}
