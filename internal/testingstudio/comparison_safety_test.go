package testingstudio

import (
	"github.com/relayops/apim/internal/store"
	"testing"
)

func TestComparisonCannotClaimMatchForMissingOrTruncatedBody(t *testing.T) {
	base := store.TestRunStep{StatusCode: 200, RedactedResponse: map[string]any{"body": map[string]any{"value": "ok"}}}
	for _, response := range []map[string]any{{}, {"body": map[string]any{"value": "ok"}, "truncated": true}} {
		cand := store.TestRunStep{StatusCode: 200, RedactedResponse: response}
		if !CompareSteps(base, cand, nil).BehavioralDiff {
			t.Fatal("incomplete response was reported as matching")
		}
	}
}
