package testingstudio

import (
	"fmt"
	"time"

	"github.com/relayops/apim/internal/store"
)

type GateEvaluation struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons"`
}

// EvaluateGateEvidence evaluates whether a completed test run satisfies an API's gate policy.
func EvaluateGateEvidence(run store.TestRun, policy *store.TestGatePolicy, currentRevision int64) GateEvaluation {
	var reasons []string
	eligible := true

	if policy == nil || !policy.EnforcementEnabled {
		return GateEvaluation{
			Eligible: true,
			Reasons:  []string{"No enforced gate policy configured for this API."},
		}
	}

	// 1. Terminal state check
	if run.LifecycleState != "completed" {
		eligible = false
		reasons = append(reasons, fmt.Sprintf("Run is in state '%s'; only 'completed' runs satisfy promotion gates.", run.LifecycleState))
	}

	// 2. Zero failed required assertions
	if run.FailedSteps > 0 || run.SkippedSteps > 0 || run.TotalSteps <= 0 || run.PassedSteps != run.TotalSteps {
		eligible = false
		reasons = append(reasons, "Run must contain at least one step, with every step passed and none skipped.")
	}

	// 3. Freshness check
	if run.CompletedAt != nil {
		maxAge := time.Duration(policy.FreshnessSeconds) * time.Second
		if time.Since(*run.CompletedAt) > maxAge {
			eligible = false
			reasons = append(reasons, fmt.Sprintf("Test run completed %s ago, which exceeds freshness requirement of %ds.", time.Since(*run.CompletedAt).Round(time.Second), policy.FreshnessSeconds))
		}
	} else {
		eligible = false
		reasons = append(reasons, "Run completion timestamp is missing.")
	}

	// 4. Target revision check
	if currentRevision <= 0 || run.ActualRevision != currentRevision {
		eligible = false
		reasons = append(reasons, fmt.Sprintf("Run executed against revision v%d, but active cluster revision is v%d (stale evidence).", run.ActualRevision, currentRevision))
	}

	// 5. Required suites check
	if len(policy.RequiredSuiteIDs) > 0 {
		matched := false
		for _, reqID := range policy.RequiredSuiteIDs {
			if reqID == run.SuiteID {
				matched = true
				break
			}
		}
		if !matched {
			eligible = false
			reasons = append(reasons, fmt.Sprintf("Suite '%s' is not in the list of required gate suites (%v).", run.SuiteID, policy.RequiredSuiteIDs))
		}
	}

	if eligible {
		reasons = append(reasons, fmt.Sprintf("Run %s passed all %d assertions within the last %ds against revision v%d.", run.ID, run.PassedSteps, policy.FreshnessSeconds, run.ActualRevision))
	}

	return GateEvaluation{
		Eligible: eligible,
		Reasons:  reasons,
	}
}
