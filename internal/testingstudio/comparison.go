package testingstudio

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/relayops/apim/internal/store"
)

type StepComparison struct {
	StepIndex          int         `json:"step_index"`
	RequestName        string      `json:"request_name"`
	BaselineRevision   int64       `json:"baseline_revision"`
	CandidateRevision  int64       `json:"candidate_revision"`
	BaselineStatus     int         `json:"baseline_status"`
	CandidateStatus    int         `json:"candidate_status"`
	StatusMatch        bool        `json:"status_match"`
	BaselineLatencyMS  float64     `json:"baseline_latency_ms"`
	CandidateLatencyMS float64     `json:"candidate_latency_ms"`
	Diffs              []FieldDiff `json:"diffs"`
	BehavioralDiff     bool        `json:"behavioral_diff"`
	Explanation        string      `json:"explanation"`
	ComparedHeaders    []string    `json:"compared_headers"`
	LatencyGateEnabled bool        `json:"latency_gate_enabled"`
	LatencyRegression  bool        `json:"latency_regression"`
}

type FieldDiff struct {
	Field     string `json:"field"` // e.g. "status", "header:content-type", "body:/data/count"
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
	Ignored   bool   `json:"ignored"`
}

// CompareSteps compares a baseline step execution with a candidate step execution.
func CompareSteps(base, cand store.TestRunStep, ignorePaths []string) StepComparison {
	return CompareStepsWithPolicy(base, cand, ignorePaths, store.ComparisonPolicy{})
}

func CompareStepsWithPolicy(base, cand store.TestRunStep, ignorePaths []string, policy store.ComparisonPolicy) StepComparison {
	comp := StepComparison{
		StepIndex:          base.StepIndex,
		RequestName:        base.RequestName,
		BaselineRevision:   base.ObservedRevision,
		CandidateRevision:  cand.ObservedRevision,
		BaselineStatus:     base.StatusCode,
		CandidateStatus:    cand.StatusCode,
		StatusMatch:        base.StatusCode == cand.StatusCode,
		BaselineLatencyMS:  base.DurationMS,
		CandidateLatencyMS: cand.DurationMS,
		Diffs:              []FieldDiff{},
	}

	if !comp.StatusMatch {
		comp.Diffs = append(comp.Diffs, FieldDiff{
			Field:     "status_code",
			Baseline:  fmt.Sprintf("%d", base.StatusCode),
			Candidate: fmt.Sprintf("%d", cand.StatusCode),
			Ignored:   false,
		})
		comp.BehavioralDiff = true
	}

	headers := policy.Headers
	if len(headers) == 0 {
		headers = []string{"content-type"}
	}
	selected := map[string]bool{}
	for _, h := range headers {
		selected[strings.ToLower(strings.TrimSpace(h))] = true
	}
	for h := range selected {
		comp.ComparedHeaders = append(comp.ComparedHeaders, h)
	}
	sort.Strings(comp.ComparedHeaders)
	bh, ch := capturedHeaders(base.RedactedResponse), capturedHeaders(cand.RedactedResponse)
	for _, h := range comp.ComparedHeaders {
		bv, bok := bh[h]
		cv, cok := ch[h]
		if bv != cv || bok != cok {
			if !bok {
				bv = "<missing>"
			}
			if !cok {
				cv = "<missing>"
			}
			comp.Diffs = append(comp.Diffs, FieldDiff{Field: "header:" + h, Baseline: bv, Candidate: cv})
			comp.BehavioralDiff = true
		}
	}
	comp.LatencyGateEnabled = policy.MaxLatencyIncreasePercent > 0
	if comp.LatencyGateEnabled {
		delta := cand.DurationMS - base.DurationMS
		allowed := base.DurationMS * policy.MaxLatencyIncreasePercent / 100
		if base.DurationMS <= 0 || cand.DurationMS <= 0 {
			comp.BehavioralDiff = true
			comp.Diffs = append(comp.Diffs, FieldDiff{Field: "latency_capture", Baseline: "positive duration required", Candidate: "timing unavailable"})
		} else if delta > allowed && delta > policy.MinLatencyIncreaseMS {
			comp.LatencyRegression = true
			comp.BehavioralDiff = true
			comp.Diffs = append(comp.Diffs, FieldDiff{Field: "latency_ms", Baseline: fmt.Sprintf("%.3f", base.DurationMS), Candidate: fmt.Sprintf("%.3f", cand.DurationMS)})
		}
	}
	ignoreMap := make(map[string]bool)
	for _, ip := range ignorePaths {
		if clean := strings.TrimSpace(ip); clean != "" {
			ignoreMap[clean] = true
		}
	}

	// Compare Response Bodies
	baseBody, hasBase := base.RedactedResponse["body"]
	candBody, hasCand := cand.RedactedResponse["body"]

	if base.RedactedResponse["truncated"] == true || cand.RedactedResponse["truncated"] == true {
		comp.BehavioralDiff = true
		comp.Diffs = append(comp.Diffs, FieldDiff{Field: "response_capture", Baseline: "incomplete comparison", Candidate: "response exceeds preview limit"})
	}
	if hasBase || hasCand {
		parsedBase := baseBody
		if str, ok := baseBody.(string); ok && (strings.HasPrefix(strings.TrimSpace(str), "{") || strings.HasPrefix(strings.TrimSpace(str), "[")) {
			var unmarshaled any
			if err := json.Unmarshal([]byte(str), &unmarshaled); err == nil {
				parsedBase = unmarshaled
			}
		}
		parsedCand := candBody
		if str, ok := candBody.(string); ok && (strings.HasPrefix(strings.TrimSpace(str), "{") || strings.HasPrefix(strings.TrimSpace(str), "[")) {
			var unmarshaled any
			if err := json.Unmarshal([]byte(str), &unmarshaled); err == nil {
				parsedCand = unmarshaled
			}
		}

		bodyDiffs := compareJSONNodes("", parsedBase, parsedCand, ignoreMap)
		comp.Diffs = append(comp.Diffs, bodyDiffs...)
		for _, d := range bodyDiffs {
			if !d.Ignored {
				comp.BehavioralDiff = true
			}
		}
	}

	if comp.BehavioralDiff {
		if !comp.StatusMatch {
			comp.Explanation = fmt.Sprintf("HTTP Status changed from %d on baseline to %d on candidate.", base.StatusCode, cand.StatusCode)
		} else {
			comp.Explanation = fmt.Sprintf("Candidate differs on %d non-ignored checks (status, selected headers, body or configured timing).", countUnignoredDiffs(comp.Diffs))
		}
	} else {
		comp.Explanation = "No differences in the checked status, selected headers and captured body; timing is gated only when configured."
	}

	sort.SliceStable(comp.Diffs, func(i, j int) bool { return comp.Diffs[i].Field < comp.Diffs[j].Field })
	return comp
}

func countUnignoredDiffs(diffs []FieldDiff) int {
	c := 0
	for _, d := range diffs {
		if !d.Ignored {
			c++
		}
	}
	return c
}

func compareJSONNodes(prefix string, a, b any, ignoreMap map[string]bool) []FieldDiff {
	var diffs []FieldDiff

	path := prefix
	if path == "" {
		path = "/"
	}

	isIgnored := ignoredPointer(prefix, ignoreMap)

	if a == nil && b == nil {
		return diffs
	}
	if a == nil || b == nil {
		diffs = append(diffs, FieldDiff{
			Field:     path,
			Baseline:  fmt.Sprintf("%v", a),
			Candidate: fmt.Sprintf("%v", b),
			Ignored:   isIgnored,
		})
		return diffs
	}

	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		diffs = append(diffs, FieldDiff{
			Field:     path,
			Baseline:  fmt.Sprintf("%v (%T)", a, a),
			Candidate: fmt.Sprintf("%v (%T)", b, b),
			Ignored:   isIgnored,
		})
		return diffs
	}

	switch valA := a.(type) {
	case map[string]any:
		valB := b.(map[string]any)
		allKeys := make(map[string]bool)
		for k := range valA {
			allKeys[k] = true
		}
		for k := range valB {
			allKeys[k] = true
		}

		for k := range allKeys {
			childPath := prefix + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
			vA, hasA := valA[k]
			vB, hasB := valB[k]

			if hasA && !hasB {
				diffs = append(diffs, FieldDiff{
					Field:     childPath,
					Baseline:  fmt.Sprintf("%v", vA),
					Candidate: "<missing>",
					Ignored:   ignoredPointer(childPath, ignoreMap),
				})
			} else if !hasA && hasB {
				diffs = append(diffs, FieldDiff{
					Field:     childPath,
					Baseline:  "<missing>",
					Candidate: fmt.Sprintf("%v", vB),
					Ignored:   ignoredPointer(childPath, ignoreMap),
				})
			} else {
				diffs = append(diffs, compareJSONNodes(childPath, vA, vB, ignoreMap)...)
			}
		}

	case []any:
		valB := b.([]any)
		if len(valA) != len(valB) {
			diffs = append(diffs, FieldDiff{
				Field:     path + "#length",
				Baseline:  fmt.Sprintf("%d items", len(valA)),
				Candidate: fmt.Sprintf("%d items", len(valB)),
				Ignored:   isIgnored,
			})
		}
		minLen := len(valA)
		if len(valB) < minLen {
			minLen = len(valB)
		}
		for i := 0; i < minLen; i++ {
			childPath := fmt.Sprintf("%s/%d", prefix, i)
			diffs = append(diffs, compareJSONNodes(childPath, valA[i], valB[i], ignoreMap)...)
		}

	default:
		strA := fmt.Sprintf("%v", a)
		strB := fmt.Sprintf("%v", b)
		if strA != strB {
			diffs = append(diffs, FieldDiff{
				Field:     path,
				Baseline:  strA,
				Candidate: strB,
				Ignored:   isIgnored,
			})
		}
	}

	return diffs
}

func ignoredPointer(path string, ignores map[string]bool) bool {
	for p := range ignores {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func capturedHeaders(response map[string]any) map[string]string {
	out := map[string]string{}
	switch headers := response["headers"].(type) {
	case map[string]string:
		for k, v := range headers {
			out[strings.ToLower(k)] = v
		}
	case map[string]any:
		for k, v := range headers {
			if value, ok := v.(string); ok {
				out[strings.ToLower(k)] = value
			}
		}
	}
	return out
}
