package ai

import (
	"testing"
)

func TestStreamMeterFragmentedSSE(t *testing.T) {
	meter := NewStreamMeter()
	meter.PromptTokens = 12

	// Fragmented across arbitrary TCP chunk boundaries
	chunks := [][]byte{
		[]byte("data: {\"model\":\"gpt-4o-mini\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"),
		[]byte("data: {\"choices\":[{\"delta\":{\"content\":\" world! How are\"}}]}\n"),
		[]byte("\ndata: {\"choices\":[{\"delta\":{\"content\":\" you doing today?\"}}]}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}

	for _, chunk := range chunks {
		meter.Feed(chunk)
	}
	meter.Finish()

	if meter.Model != "gpt-4o-mini" {
		t.Fatalf("expected model gpt-4o-mini, got %q", meter.Model)
	}
	if meter.CompletionTokens <= 0 {
		t.Fatalf("expected estimated completion tokens > 0, got %d", meter.CompletionTokens)
	}
	if meter.Provenance != ProvenanceEstimated {
		t.Fatalf("expected ProvenanceEstimated, got %q", meter.Provenance)
	}
	if meter.TotalTokens != 12+meter.CompletionTokens {
		t.Fatalf("expected total tokens %d, got %d", 12+meter.CompletionTokens, meter.TotalTokens)
	}
}

func TestStreamMeterAuthoritativeProviderUsage(t *testing.T) {
	meter := NewStreamMeter()

	chunks := [][]byte{
		[]byte("data: {\"model\":\"claude-3-5-sonnet\",\"choices\":[{\"delta\":{\"content\":\"Thinking...\"}}]}\n\n"),
		// Final stream chunk containing authoritative usage
		[]byte("data: {\"model\":\"claude-3-5-sonnet\",\"choices\":[],\"usage\":{\"prompt_tokens\":25,\"completion_tokens\":50,\"total_tokens\":75}}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}

	for _, chunk := range chunks {
		meter.Feed(chunk)
	}
	meter.Finish()

	if meter.Model != "claude-3-5-sonnet" {
		t.Fatalf("expected model claude-3-5-sonnet, got %q", meter.Model)
	}
	if meter.PromptTokens != 25 {
		t.Fatalf("expected prompt tokens 25, got %d", meter.PromptTokens)
	}
	if meter.CompletionTokens != 50 {
		t.Fatalf("expected completion tokens 50, got %d", meter.CompletionTokens)
	}
	if meter.TotalTokens != 75 {
		t.Fatalf("expected total tokens 75, got %d", meter.TotalTokens)
	}
	if meter.Provenance != ProvenanceProviderReported {
		t.Fatalf("expected ProvenanceProviderReported, got %q", meter.Provenance)
	}
}

func TestStreamMeterStandardJSON(t *testing.T) {
	meter := NewStreamMeter()

	payload := []byte(`{
		"id": "chatcmpl-123",
		"model": "meta-llama-3.1-70b",
		"choices": [{
			"message": {"role": "assistant", "content": "Sure, here is the answer."}
		}],
		"usage": {
			"prompt_tokens": 15,
			"completion_tokens": 8,
			"total_tokens": 23
		}
	}`)

	// Feed in two chunks
	meter.Feed(payload[:20])
	meter.Feed(payload[20:])
	meter.Finish()

	if meter.Model != "meta-llama-3.1-70b" {
		t.Fatalf("expected model meta-llama-3.1-70b, got %q", meter.Model)
	}
	if meter.PromptTokens != 15 || meter.CompletionTokens != 8 || meter.TotalTokens != 23 {
		t.Fatalf("unexpected token counts: prompt=%d, comp=%d, total=%d", meter.PromptTokens, meter.CompletionTokens, meter.TotalTokens)
	}
	if meter.Provenance != ProvenanceProviderReported {
		t.Fatalf("expected ProvenanceProviderReported, got %q", meter.Provenance)
	}
}
