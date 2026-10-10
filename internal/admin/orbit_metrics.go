package admin

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/orbit"
)

// orbitMetrics counts Orbit usage for Prometheus. Model name and token use
// belong in observability, not in the assistant's answers.
type orbitMetrics struct {
	answered, failed, rateLimited atomic.Int64
	promptTokens, completionTok   atomic.Int64
	toolCalls, toolFailures       atomic.Int64
	proposalsDrafted, applied     atomic.Int64
	answerMillis                  atomic.Int64 // sum over answered questions
}

func (m *orbitMetrics) observe(ans orbit.Answer, err error, took time.Duration, drafted int) {
	if err != nil {
		m.failed.Add(1)
	} else {
		m.answered.Add(1)
		m.answerMillis.Add(took.Milliseconds())
	}
	m.promptTokens.Add(int64(ans.Usage.PromptTokens))
	m.completionTok.Add(int64(ans.Usage.CompletionTokens))
	for _, st := range ans.Steps {
		m.toolCalls.Add(1)
		if !st.OK {
			m.toolFailures.Add(1)
		}
	}
	m.proposalsDrafted.Add(int64(drafted))
}

func (s *Server) writeOrbitMetrics(w io.Writer) {
	if s.orbit == nil {
		return
	}
	m := &s.orbitStats
	model := s.orbit.client.Model()
	fmt.Fprintf(w, "# HELP relayops_orbit_questions_total Questions asked of Orbit AI by outcome\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_questions_total counter\n")
	fmt.Fprintf(w, "relayops_orbit_questions_total{outcome=\"answered\"} %d\n", m.answered.Load())
	fmt.Fprintf(w, "relayops_orbit_questions_total{outcome=\"failed\"} %d\n", m.failed.Load())
	fmt.Fprintf(w, "relayops_orbit_questions_total{outcome=\"rate_limited\"} %d\n\n", m.rateLimited.Load())
	fmt.Fprintf(w, "# HELP relayops_orbit_tokens_total Model tokens used by Orbit AI\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_tokens_total counter\n")
	fmt.Fprintf(w, "relayops_orbit_tokens_total{model=%q,kind=\"prompt\"} %d\n", model, m.promptTokens.Load())
	fmt.Fprintf(w, "relayops_orbit_tokens_total{model=%q,kind=\"completion\"} %d\n\n", model, m.completionTok.Load())
	fmt.Fprintf(w, "# HELP relayops_orbit_answer_seconds Time to answer a question (sum and count of answered questions)\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_answer_seconds summary\n")
	fmt.Fprintf(w, "relayops_orbit_answer_seconds_sum %.3f\n", float64(m.answerMillis.Load())/1000)
	fmt.Fprintf(w, "relayops_orbit_answer_seconds_count %d\n\n", m.answered.Load())
	fmt.Fprintf(w, "# HELP relayops_orbit_tool_calls_total Tool calls Orbit made, and how many failed (for example permission denied)\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_tool_calls_total counter\n")
	fmt.Fprintf(w, "relayops_orbit_tool_calls_total{result=\"ok\"} %d\n", m.toolCalls.Load()-m.toolFailures.Load())
	fmt.Fprintf(w, "relayops_orbit_tool_calls_total{result=\"error\"} %d\n\n", m.toolFailures.Load())
	fmt.Fprintf(w, "# HELP relayops_orbit_proposals_total Changes Orbit drafted and changes people applied\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_proposals_total counter\n")
	fmt.Fprintf(w, "relayops_orbit_proposals_total{stage=\"drafted\"} %d\n", m.proposalsDrafted.Load())
	fmt.Fprintf(w, "relayops_orbit_proposals_total{stage=\"applied\"} %d\n\n", m.applied.Load())
}
