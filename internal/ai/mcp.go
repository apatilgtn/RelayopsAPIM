package ai

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrToolNotAllowed        = errors.New("tool execution forbidden by agent grant allowlist")
	ErrToolCallLimitExceeded = errors.New("agent tool calls exceeded maximum allowed quota per run")
	ErrApprovalRequired      = errors.New("consequential tool execution requires human approval")
	ErrInvalidApprovalToken  = errors.New("human approval token is invalid, expired, or tampered")
)

// ToolExecutionRequest represents an invocation of an MCP or API tool by an autonomous agent.
type ToolExecutionRequest struct {
	AgentName     string         `json:"agent_name"`
	ToolName      string         `json:"tool_name"`
	Arguments     map[string]any `json:"arguments"`
	CallIndex     int            `json:"call_index"`
	ApprovalToken string         `json:"approval_token,omitempty"`
}

// ApprovalClaims defines the payload bound to an action approval token.
type ApprovalClaims struct {
	AgentName string
	ToolName  string
	Nonce     string
	ExpiresAt time.Time
}

// AgentGovernor enforces agent tool grants, quotas, and human approval verification.
type AgentGovernor struct {
	signingKey []byte
}

// NewAgentGovernor initializes an agent tool governor.
func NewAgentGovernor(secret string) *AgentGovernor {
	if secret == "" {
		secret = "relayops-default-mcp-governor-key"
	}
	return &AgentGovernor{
		signingKey: []byte(secret),
	}
}

// AuthorizeToolCall validates whether the agent is entitled to invoke the requested tool.
func (g *AgentGovernor) AuthorizeToolCall(allowedTools []string, maxCalls int, req ToolExecutionRequest) error {
	// 1. Tool allowlist check
	allowed := false
	for _, tool := range allowedTools {
		if tool == "*" || strings.EqualFold(tool, req.ToolName) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: tool '%s' is not in allowed list %v", ErrToolNotAllowed, req.ToolName, allowedTools)
	}

	// 2. Max call count check
	if maxCalls > 0 && req.CallIndex > maxCalls {
		return fmt.Errorf("%w: call %d exceeds maximum %d", ErrToolCallLimitExceeded, req.CallIndex, maxCalls)
	}

	return nil
}

// GenerateApprovalToken mints a cryptographically signed approval token for a high-risk tool call.
func (g *AgentGovernor) GenerateApprovalToken(agentName, toolName, nonce string, ttl time.Duration) string {
	expiresAt := time.Now().Add(ttl).Unix()
	msg := fmt.Sprintf("%s|%s|%s|%d", agentName, toolName, nonce, expiresAt)

	mac := hmac.New(sha256.New, g.signingKey)
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))

	return fmt.Sprintf("%s|%s", msg, sig)
}

// ValidateApprovalToken verifies the authenticity and expiration of a human approval token.
func (g *AgentGovernor) ValidateApprovalToken(token, expectedAgent, expectedTool string) error {
	parts := strings.Split(token, "|")
	if len(parts) != 5 {
		return ErrInvalidApprovalToken
	}

	agentName := parts[0]
	toolName := parts[1]
	nonce := parts[2]
	expiryStr := parts[3]
	claimedSig := parts[4]

	if agentName != expectedAgent || toolName != expectedTool {
		return fmt.Errorf("%w: token bound to %s.%s, expected %s.%s", ErrInvalidApprovalToken, agentName, toolName, expectedAgent, expectedTool)
	}

	msg := fmt.Sprintf("%s|%s|%s|%s", agentName, toolName, nonce, expiryStr)
	mac := hmac.New(sha256.New, g.signingKey)
	mac.Write([]byte(msg))
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(claimedSig), []byte(expectedSig)) {
		return ErrInvalidApprovalToken
	}

	var expiryUnix int64
	_, _ = fmt.Sscanf(expiryStr, "%d", &expiryUnix)
	if time.Now().Unix() > expiryUnix {
		return fmt.Errorf("%w: approval expired", ErrInvalidApprovalToken)
	}

	return nil
}
