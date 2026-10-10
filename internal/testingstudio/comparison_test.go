package testingstudio

import (
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestCompareSteps_Identical(t *testing.T) {
	respBody := `{"id": "user-123", "status": "active"}`
	bStep := store.TestRunStep{
		StepIndex:        1,
		RequestName:      "Get User",
		ObservedRevision: 1,
		StatusCode:       200,
		RedactedResponse: map[string]any{
			"status":  200,
			"body":    respBody,
			"headers": map[string]string{"Content-Type": "application/json"},
		},
		DurationMS: 10.0,
	}
	cStep := store.TestRunStep{
		StepIndex:        1,
		RequestName:      "Get User",
		ObservedRevision: 2,
		StatusCode:       200,
		RedactedResponse: map[string]any{
			"status":  200,
			"body":    respBody,
			"headers": map[string]string{"Content-Type": "application/json"},
		},
		DurationMS: 12.0,
	}

	comp := CompareSteps(bStep, cStep, nil)
	if comp.BehavioralDiff {
		t.Fatalf("Expected identical steps to match without diff, got diffs: %+v", comp.Diffs)
	}
}

func TestCompareSteps_StatusMismatch(t *testing.T) {
	bStep := store.TestRunStep{
		StepIndex:        1,
		RequestName:      "Checkout",
		ObservedRevision: 1,
		StatusCode:       200,
		RedactedResponse: map[string]any{"status": 200, "body": `{"ok": true}`},
	}
	cStep := store.TestRunStep{
		StepIndex:        1,
		RequestName:      "Checkout",
		ObservedRevision: 2,
		StatusCode:       500,
		RedactedResponse: map[string]any{"status": 500, "body": `{"error": "internal_error"}`},
	}

	comp := CompareSteps(bStep, cStep, nil)
	if !comp.BehavioralDiff {
		t.Fatalf("Expected behavioral diff for differing status codes")
	}
	if len(comp.Diffs) == 0 {
		t.Fatalf("Expected recorded differences, got 0")
	}
}

func TestCompareSteps_IgnoreJSONPaths(t *testing.T) {
	bStep := store.TestRunStep{
		StepIndex:        1,
		RequestName:      "Get Timestamp",
		ObservedRevision: 1,
		StatusCode:       200,
		RedactedResponse: map[string]any{
			"status": 200,
			"body":   `{"id": "fixed-id", "timestamp": "2026-10-05T12:00:00Z", "uuid": "abc-123"}`,
		},
	}
	cStep := store.TestRunStep{
		StepIndex:        1,
		RequestName:      "Get Timestamp",
		ObservedRevision: 2,
		StatusCode:       200,
		RedactedResponse: map[string]any{
			"status": 200,
			"body":   `{"id": "fixed-id", "timestamp": "2026-10-05T12:00:01Z", "uuid": "def-456"}`,
		},
	}

	// Without ignore list: should report behavioral diff
	compNoIgnore := CompareSteps(bStep, cStep, nil)
	if !compNoIgnore.BehavioralDiff {
		t.Fatalf("Expected behavioral diff when ignore paths are empty")
	}

	// With ignore list: should NOT report behavioral diff!
	ignoreList := []string{"/timestamp", "/uuid"}
	compIgnored := CompareSteps(bStep, cStep, ignoreList)
	if compIgnored.BehavioralDiff {
		t.Fatalf("Expected no behavioral diff with ignored paths, but got: %+v", compIgnored.Diffs)
	}
}
