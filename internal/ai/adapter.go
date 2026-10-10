package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ProviderType identifies the upstream AI inference provider protocol.
type ProviderType string

const (
	ProviderOpenAI    ProviderType = "openai"
	ProviderAnthropic ProviderType = "anthropic"
	ProviderOllama    ProviderType = "ollama"
	ProviderAzure     ProviderType = "azure_openai"
	ProviderCustom    ProviderType = "custom"
)

// ChatRequest represents the normalized standard chat completion request payload.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// NormalizeChatRequest parses and validates an incoming chat completion request.
func NormalizeChatRequest(bodyBytes []byte) (*ChatRequest, error) {
	if len(bodyBytes) == 0 {
		return nil, errors.New("empty request body")
	}

	var req ChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, fmt.Errorf("invalid chat completion JSON: %w", err)
	}

	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		return nil, errors.New("model field is required in chat request")
	}

	if len(req.Messages) == 0 {
		return nil, errors.New("messages array must contain at least one message")
	}

	return &req, nil
}

// BuildAuthHeader resolves upstream credentials for the provider type.
func BuildAuthHeader(providerType ProviderType, apiKey string) (headerName, headerValue string) {
	apiKey = strings.TrimSpace(apiKey)
	switch providerType {
	case ProviderAnthropic:
		return "x-api-key", apiKey
	case ProviderAzure:
		return "api-key", apiKey
	default:
		// OpenAI, Ollama, Custom standard Bearer auth
		if !strings.HasPrefix(apiKey, "Bearer ") {
			return "Authorization", "Bearer " + apiKey
		}
		return "Authorization", apiKey
	}
}

// ValidateModelCapability checks whether a requested feature (e.g. streaming, tools) is supported by the deployment.
func ValidateModelCapability(capabilities map[string]any, feature string) bool {
	if capabilities == nil {
		return true // permissive default if not specified
	}
	val, exists := capabilities[feature]
	if !exists {
		return true
	}
	b, ok := val.(bool)
	return !ok || b
}
