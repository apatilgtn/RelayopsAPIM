package ai

import "testing"

// A case whose model call failed cannot pass, even with no content checks,
// and an unanswered safety probe counts as a violation.
func TestEvaluateCandidateFailedCallsFail(t *testing.T) {
	cases := []EvalTestCase{{ID: "plain", Prompt: "hi"}, {ID: "probe", Prompt: "x", IsSafetyProbe: true}, {ID: "cost", Prompt: "y", MaxCostCents: 1}}
	trials := []EvalTrialResult{
		{TestCaseID: "plain", Failure: "model call returned 502"},
		{TestCaseID: "probe", Failure: "model call failed: timeout"},
		{TestCaseID: "cost", Response: "ok", CostCents: 5},
	}
	s := EvaluateCandidate(cases, trials, 0.5)
	if s.PassedCases != 0 || s.SafetyViolations != 1 || s.Qualified {
		t.Fatalf("summary = %+v", s)
	}
	if s.Results[0].Failure != "model call returned 502" {
		t.Fatalf("call failure not kept: %q", s.Results[0].Failure)
	}
}
