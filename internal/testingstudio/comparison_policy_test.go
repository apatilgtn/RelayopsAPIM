package testingstudio

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relayops/apim/internal/store"
)

func TestComparisonPolicyHeadersAndTiming(t *testing.T) {
	base := store.TestRunStep{StatusCode: 200, DurationMS: 100, RedactedResponse: map[string]any{"body": `{"ok":true}`, "headers": map[string]string{"Content-Type": "application/json", "Date": "yesterday"}}}
	cand := base
	cand.DurationMS = 180
	cand.RedactedResponse = map[string]any{"body": `{"ok":true}`, "headers": map[string]any{"content-type": "application/json", "Date": "today"}}
	if CompareSteps(base, cand, nil).BehavioralDiff {
		t.Fatal("dynamic unselected header or disabled timing gate affected result")
	}
	policy := store.ComparisonPolicy{MaxLatencyIncreasePercent: 50, MinLatencyIncreaseMS: 100}
	if CompareStepsWithPolicy(base, cand, nil, policy).BehavioralDiff {
		t.Fatal("noise floor was not respected")
	}
	policy.MinLatencyIncreaseMS = 20
	comp := CompareStepsWithPolicy(base, cand, nil, policy)
	if !comp.BehavioralDiff || !comp.LatencyRegression {
		t.Fatal("latency regression was not detected")
	}
	cand.DurationMS = 100
	cand.RedactedResponse["headers"] = map[string]any{"content-type": "text/plain"}
	if !CompareSteps(base, cand, nil).BehavioralDiff {
		t.Fatal("content type regression was not detected")
	}
	cand.RedactedResponse["headers"] = map[string]any{}
	if !CompareSteps(base, cand, nil).BehavioralDiff {
		t.Fatal("removed selected header was not detected")
	}
	cand.RedactedResponse = base.RedactedResponse
	cand.DurationMS = 0
	if !CompareStepsWithPolicy(base, cand, nil, policy).BehavioralDiff {
		t.Fatal("missing timing satisfied enabled gate")
	}
}

func TestComparisonPointersAndStableOrdering(t *testing.T) {
	base := store.TestRunStep{StatusCode: 200, RedactedResponse: map[string]any{"body": `{"timestamp":1,"Timestamp":1,"nested":{"timestamp":1},"a/b":{"~key":1},"z":1}`}}
	cand := store.TestRunStep{StatusCode: 200, RedactedResponse: map[string]any{"body": `{"timestamp":2,"Timestamp":2,"nested":{"timestamp":2},"a/b":{"~key":2},"z":2}`}}
	comp := CompareSteps(base, cand, []string{"/timestamp", "/a~1b/~0key"})
	ignored := map[string]bool{}
	for _, d := range comp.Diffs {
		ignored[d.Field] = d.Ignored
	}
	if !ignored["/timestamp"] || !ignored["/a~1b/~0key"] || ignored["/Timestamp"] || ignored["/nested/timestamp"] {
		t.Fatalf("pointer scope is incorrect: %+v", comp.Diffs)
	}
	raw, _ := json.Marshal(comp)
	for i := 0; i < 50; i++ {
		next, _ := json.Marshal(CompareSteps(base, cand, []string{"/timestamp", "/a~1b/~0key"}))
		if string(next) != string(raw) {
			t.Fatal("diff ordering is unstable")
		}
	}
	if !CompareSteps(base, cand, nil).BehavioralDiff {
		t.Fatal("changes silently ignored")
	}
	for _, d := range CompareSteps(base, cand, []string{"/nested"}).Diffs {
		if strings.HasPrefix(d.Field, "/nested/") && !d.Ignored {
			t.Fatal("explicit subtree ignore failed")
		}
	}
}

func TestComparisonPolicyValidation(t *testing.T) {
	valid := store.SuiteDefinition{Name: "policy", Requests: []store.RequestDef{{Name: "read", Method: "GET", Path: "/"}}}
	for _, h := range []string{"authorization", "set-cookie", "x-relayops-revision", "bad header"} {
		def := valid
		def.Comparison.Headers = []string{h}
		if ValidateSuiteDefinition(def) == nil {
			t.Fatalf("unsafe header accepted: %s", h)
		}
	}
	for _, p := range []string{"timestamp", "/bad~escape", ""} {
		def := valid
		def.IgnorePaths = []string{p}
		if ValidateSuiteDefinition(def) == nil {
			t.Fatalf("invalid pointer accepted: %q", p)
		}
	}
	def := valid
	def.Comparison.MinLatencyIncreaseMS = 10
	if ValidateSuiteDefinition(def) == nil {
		t.Fatal("inactive percentage with timing floor accepted")
	}
	def.Comparison.MaxLatencyIncreasePercent = 25
	def.IgnorePaths = []string{"/a~1b/~0key"}
	def.Comparison.Headers = []string{"content-type", "cache-control"}
	if err := ValidateSuiteDefinition(def); err != nil {
		t.Fatal(err)
	}
	def.Comparison.MaxLatencyIncreasePercent = -1
	if ValidateSuiteDefinition(def) == nil {
		t.Fatal("negative threshold accepted")
	}
}
