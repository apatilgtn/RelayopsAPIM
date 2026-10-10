package admin

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/relayops/apim/internal/orbit"
)

// orbitMetrics counts Orbit usage for Prometheus. Model name and token use
// belong in observability, not in the assistant's answers.
type orbitMetrics struct {
	answered, failed, rateLimited atomic.Int64
	fallbacks                     atomic.Int64 // answered by the fallback model
	toolCalls, toolFailures       atomic.Int64
	proposalsDrafted, applied     atomic.Int64
	answerMillis                  atomic.Int64 // sum over answered questions

	tokensMu sync.Mutex
	tokens   map[string]*[2]int64 // model -> prompt, completion
}

func (m *orbitMetrics) observe(ans orbit.Answer, err error, took time.Duration, drafted int) {
	if err != nil {
		m.failed.Add(1)
	} else {
		m.answered.Add(1)
		m.answerMillis.Add(took.Milliseconds())
		if ans.Fallback {
			m.fallbacks.Add(1)
		}
	}
	if ans.Model != "" && ans.Usage.PromptTokens+ans.Usage.CompletionTokens > 0 {
		m.tokensMu.Lock()
		if m.tokens == nil {
			m.tokens = map[string]*[2]int64{}
		}
		t := m.tokens[ans.Model]
		if t == nil {
			t = &[2]int64{}
			m.tokens[ans.Model] = t
		}
		t[0] += int64(ans.Usage.PromptTokens)
		t[1] += int64(ans.Usage.CompletionTokens)
		m.tokensMu.Unlock()
	}
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
	fmt.Fprintf(w, "# HELP relayops_orbit_questions_total Questions asked of Orbit AI by outcome\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_questions_total counter\n")
	fmt.Fprintf(w, "relayops_orbit_questions_total{outcome=\"answered\"} %d\n", m.answered.Load())
	fmt.Fprintf(w, "relayops_orbit_questions_total{outcome=\"failed\"} %d\n", m.failed.Load())
	fmt.Fprintf(w, "relayops_orbit_questions_total{outcome=\"rate_limited\"} %d\n\n", m.rateLimited.Load())
	fmt.Fprintf(w, "# HELP relayops_orbit_fallback_answers_total Questions answered by the fallback model because the primary was unavailable\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_fallback_answers_total counter\n")
	fmt.Fprintf(w, "relayops_orbit_fallback_answers_total %d\n\n", m.fallbacks.Load())
	fmt.Fprintf(w, "# HELP relayops_orbit_tokens_total Model tokens used by Orbit AI\n")
	fmt.Fprintf(w, "# TYPE relayops_orbit_tokens_total counter\n")
	m.tokensMu.Lock()
	if m.tokens == nil {
		m.tokens = map[string]*[2]int64{}
	}
	if _, ok := m.tokens[s.orbit.client.Model()]; !ok {
		m.tokens[s.orbit.client.Model()] = &[2]int64{} // configured model always reported, even at zero
	}
	models := make([]string, 0, len(m.tokens))
	for k := range m.tokens {
		models = append(models, k)
	}
	sort.Strings(models)
	for _, model := range models {
		t := m.tokens[model]
		fmt.Fprintf(w, "relayops_orbit_tokens_total{model=%q,kind=\"prompt\"} %d\n", model, t[0])
		fmt.Fprintf(w, "relayops_orbit_tokens_total{model=%q,kind=\"completion\"} %d\n", model, t[1])
	}
	m.tokensMu.Unlock()
	fmt.Fprintln(w)
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
