package testingstudio

import (
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func TestEvaluateGateEligibility(t *testing.T) {
	now := time.Now()
	apiID := "api-1"
	suiteID := "suite-1"

	policy := store.TestGatePolicy{
		APIID:              &apiID,
		RequiredSuiteIDs:   []string{suiteID},
		FreshnessSeconds:   3600,
		EnforcementEnabled: true,
	}

	targetRev := int64(10)

	// Case 1: Valid passing run
	passingRun := store.TestRun{
		ID:             "run-1",
		SuiteID:        suiteID,
		LifecycleState: "completed",
		PassedSteps:    5,
		TotalSteps:     5,
		FailedSteps:    0,
		ActualRevision: targetRev,
		CompletedAt:    &now,
	}
	evPassing := EvaluateGateEvidence(passingRun, &policy, targetRev)
	if !evPassing.Eligible {
		t.Fatalf("Expected eligible evidence, got ineligible: %+v", evPassing.Reasons)
	}

	// Case 2: Failed step in run
	failedRun := passingRun
	failedRun.FailedSteps = 1
	evFailed := EvaluateGateEvidence(failedRun, &policy, targetRev)
	if evFailed.Eligible {
		t.Fatalf("Expected ineligible evidence for run with failed steps")
	}

	// Case 3: Expired evidence
	oldTime := now.Add(-2 * time.Hour)
	expiredRun := passingRun
	expiredRun.CompletedAt = &oldTime
	evExpired := EvaluateGateEvidence(expiredRun, &policy, targetRev)
	if evExpired.Eligible {
		t.Fatalf("Expected ineligible evidence for expired run")
	}

	// Case 4: Mismatched revision
	evMismatch := EvaluateGateEvidence(passingRun, &policy, int64(11))
	if evMismatch.Eligible {
		t.Fatalf("Expected ineligible evidence for mismatched target revision")
	}
	for _, test := range []string{"missing-revision", "skipped", "empty"} {
		t.Run(test, func(t *testing.T) {
			run := passingRun
			switch test {
			case "missing-revision":
				run.ActualRevision = 0
			case "skipped":
				run.SkippedSteps = 1
			case "empty":
				run.TotalSteps = 0
				run.PassedSteps = 0
			}
			if EvaluateGateEvidence(run, &policy, targetRev).Eligible {
				t.Fatal("incomplete evidence was accepted")
			}
		})
	}
}
