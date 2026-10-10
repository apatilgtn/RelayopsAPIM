package ai

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// UsageProvenance indicates the trustworthiness level of token accounting.
type UsageProvenance string

const (
	ProvenanceProviderReported UsageProvenance = "provider_reported"
	ProvenanceEstimated        UsageProvenance = "estimated"
	ProvenanceUnavailable      UsageProvenance = "unavailable"
)

// StreamMeter incrementally parses streaming SSE or non-streaming JSON responses
// from AI model providers to extract usage, model identity, and provenance.
type StreamMeter struct {
	Model               string
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	Provenance          UsageProvenance
	isSSE               bool
	buffer              bytes.Buffer
	hasCompleted        bool
	estimatedCompTokens int
}

// NewStreamMeter creates an initialized stream meter.
func NewStreamMeter() *StreamMeter {
	return &StreamMeter{
		Provenance: ProvenanceUnavailable,
	}
}

// Feed consumes an arbitrary byte chunk from the HTTP response stream.
func (m *StreamMeter) Feed(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	// Detect SSE format early
	if !m.isSSE && (bytes.Contains(chunk, []byte("data:")) || bytes.HasPrefix(chunk, []byte("event:"))) {
		m.isSSE = true
	}

	if m.isSSE {
		m.feedSSE(chunk)
	} else {
		// Buffer non-streaming JSON payload (capped at 2 MiB for usage inspection)
		if m.buffer.Len()+len(chunk) <= 2<<20 {
			m.buffer.Write(chunk)
		}
	}
}

// Finish flushes remaining buffer content and finalizes token calculations.
func (m *StreamMeter) Finish() {
	if m.isSSE {
		// Flush any remaining buffered line in SSE
		if m.buffer.Len() > 0 {
			line := m.buffer.String()
			m.buffer.Reset()
			m.processSSELine(line)
		}

		if m.Provenance != ProvenanceProviderReported && m.estimatedCompTokens > 0 {
			m.CompletionTokens = m.estimatedCompTokens
			m.TotalTokens = m.PromptTokens + m.CompletionTokens
			m.Provenance = ProvenanceEstimated
		}
	} else {
		// Parse complete JSON body
		m.parseStandardJSON(m.buffer.Bytes())
	}
}

func (m *StreamMeter) feedSSE(chunk []byte) {
	m.buffer.Write(chunk)

	for {
		buf := m.buffer.Bytes()
		newlineIdx := bytes.IndexByte(buf, '\n')
		if newlineIdx == -1 {
			break
		}

		line := string(buf[:newlineIdx])
		m.buffer.Next(newlineIdx + 1)
		m.processSSELine(line)
	}
}

func (m *StreamMeter) processSSELine(rawLine string) {
	line := strings.TrimSpace(rawLine)
	if line == "" || strings.HasPrefix(line, ":") {
		return
	}

	if !strings.HasPrefix(line, "data:") {
		return
	}

	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		m.hasCompleted = true
		return
	}

	var payload struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}

	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return
	}

	if payload.Model != "" && m.Model == "" {
		m.Model = payload.Model
	}

	// 1. Authoritative provider usage block in final chunk (e.g. OpenAI stream_options: {"include_usage": true})
	if payload.Usage != nil && (payload.Usage.TotalTokens > 0 || payload.Usage.PromptTokens > 0 || payload.Usage.CompletionTokens > 0) {
		m.PromptTokens = payload.Usage.PromptTokens
		m.CompletionTokens = payload.Usage.CompletionTokens
		m.TotalTokens = payload.Usage.TotalTokens
		if m.TotalTokens == 0 && (m.PromptTokens > 0 || m.CompletionTokens > 0) {
			m.TotalTokens = m.PromptTokens + m.CompletionTokens
		}
		m.Provenance = ProvenanceProviderReported
		return
	}

	// 2. Incremental delta content estimation fallback
	for _, choice := range payload.Choices {
		if choice.Delta.Content != "" {
			// Estimate ~4 characters or 1 token per delta piece
			chars := utf8.RuneCountInString(choice.Delta.Content)
			tokens := (chars + 3) / 4
			if tokens < 1 {
				tokens = 1
			}
			m.estimatedCompTokens += tokens
		}
	}
}

func (m *StreamMeter) parseStandardJSON(body []byte) {
	if len(body) == 0 {
		return
	}

	var payload struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}

	if payload.Model != "" {
		m.Model = payload.Model
	}

	if payload.Usage != nil {
		m.PromptTokens = payload.Usage.PromptTokens
		m.CompletionTokens = payload.Usage.CompletionTokens
		m.TotalTokens = payload.Usage.TotalTokens
		if m.TotalTokens == 0 && (m.PromptTokens > 0 || m.CompletionTokens > 0) {
			m.TotalTokens = m.PromptTokens + m.CompletionTokens
		}
		m.Provenance = ProvenanceProviderReported
	}
}
