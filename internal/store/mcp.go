package store

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// MCPPolicy governs an API whose protocol is "mcp" (a Model Context Protocol
// server behind the gateway): which tools each caller may use, the approved
// tool catalog, and per-tool call limits.
type MCPPolicy struct {
	// DefaultAction applies when no rule matches: "allow" (default) or "deny".
	DefaultAction string `json:"default_action,omitempty"`
	// Rules are evaluated in order; the first whose tools, consumers and plans
	// all match decides.
	Rules []MCPToolRule `json:"rules,omitempty"`
	// PinnedTools is the approved tool catalog: tool name -> fingerprint of its
	// definition (description, schemas, annotations). When set, tools that are
	// not pinned, or whose definition no longer matches its pin, are hidden
	// from tools/list and refused on tools/call.
	PinnedTools map[string]string `json:"pinned_tools,omitempty"`
	// PinnedPrompts is the approved prompt catalog, like PinnedTools: prompt
	// templates reach the model too, so a changed prompt is held for review.
	PinnedPrompts map[string]string `json:"pinned_prompts,omitempty"`
	// ResourceRules and PromptRules allow or deny resource URIs (globs where
	// "*" matches any characters, including "/") and prompt names. With no
	// match, DefaultAction applies.
	ResourceRules []MCPRule `json:"resource_rules,omitempty"`
	PromptRules   []MCPRule `json:"prompt_rules,omitempty"`
	// ValidateArguments checks tools/call arguments against the approved
	// tool's input schema.
	ValidateArguments bool `json:"validate_arguments,omitempty"`
	// ToolCallsPerMinute limits each caller's calls to each tool (0 = off).
	ToolCallsPerMinute int `json:"tool_calls_per_minute,omitempty"`
	// MaxBodyBytes bounds a JSON-RPC request body (default 1 MiB).
	MaxBodyBytes int `json:"max_body_bytes,omitempty"`
}

// MCPRule allows or denies resources or prompts (by URI or name pattern)
// for a set of callers. Empty consumers or plans match any caller.
type MCPRule struct {
	Match     []string `json:"match"`
	Consumers []string `json:"consumers,omitempty"`
	Plans     []string `json:"plans,omitempty"`
	Action    string   `json:"action"`
}

// MCPToolRule allows or denies tools for a set of callers. Empty consumers or
// plans match any caller.
type MCPToolRule struct {
	Tools     []string `json:"tools"`               // tool names or globs ("github_*", "*")
	Consumers []string `json:"consumers,omitempty"` // consumer IDs
	Plans     []string `json:"plans,omitempty"`     // plan names
	Action    string   `json:"action"`              // allow or deny
}

const (
	MaxMCPRules           = 100
	MaxMCPPinnedTools     = 1000
	DefaultMCPMaxBody     = 1 << 20
	MaxMCPMaxBody         = 16 << 20
	MaxMCPToolsPerMinute  = 1_000_000
	mcpFingerprintPattern = `^sha256:[0-9a-f]{64}$`
)

var mcpFingerprint = regexp.MustCompile(mcpFingerprintPattern)

// Normalize fills defaults.
func (p *MCPPolicy) Normalize() {
	p.DefaultAction = strings.ToLower(strings.TrimSpace(p.DefaultAction))
	if p.DefaultAction == "" {
		p.DefaultAction = "allow"
	}
	if p.MaxBodyBytes == 0 {
		p.MaxBodyBytes = DefaultMCPMaxBody
	}
	for i := range p.Rules {
		p.Rules[i].Action = strings.ToLower(strings.TrimSpace(p.Rules[i].Action))
	}
	for _, rules := range [][]MCPRule{p.ResourceRules, p.PromptRules} {
		for i := range rules {
			rules[i].Action = strings.ToLower(strings.TrimSpace(rules[i].Action))
		}
	}
}

// Validate reports the first invalid setting. Call Normalize first.
func (p MCPPolicy) Validate() error {
	if p.DefaultAction != "allow" && p.DefaultAction != "deny" {
		return errors.New("mcp_policy.default_action must be allow or deny")
	}
	if len(p.Rules) > MaxMCPRules {
		return fmt.Errorf("mcp_policy.rules supports at most %d rules", MaxMCPRules)
	}
	for i, r := range p.Rules {
		if r.Action != "allow" && r.Action != "deny" {
			return fmt.Errorf("mcp_policy.rules[%d].action must be allow or deny", i)
		}
		if len(r.Tools) == 0 {
			return fmt.Errorf("mcp_policy.rules[%d].tools must name at least one tool or pattern", i)
		}
		for _, t := range r.Tools {
			if _, err := path.Match(t, ""); err != nil || strings.TrimSpace(t) == "" {
				return fmt.Errorf("mcp_policy.rules[%d].tools has an invalid pattern %q", i, t)
			}
		}
	}
	for field, pins := range map[string]map[string]string{"pinned_tools": p.PinnedTools, "pinned_prompts": p.PinnedPrompts} {
		if len(pins) > MaxMCPPinnedTools {
			return fmt.Errorf("mcp_policy.%s supports at most %d entries", field, MaxMCPPinnedTools)
		}
		for name, fp := range pins {
			if name == "" || !mcpFingerprint.MatchString(fp) {
				return fmt.Errorf("mcp_policy.%s[%q] must be a fingerprint like sha256:<64 hex>", field, name)
			}
		}
	}
	for field, rules := range map[string][]MCPRule{"resource_rules": p.ResourceRules, "prompt_rules": p.PromptRules} {
		if len(rules) > MaxMCPRules {
			return fmt.Errorf("mcp_policy.%s supports at most %d rules", field, MaxMCPRules)
		}
		for i, r := range rules {
			if r.Action != "allow" && r.Action != "deny" {
				return fmt.Errorf("mcp_policy.%s[%d].action must be allow or deny", field, i)
			}
			if len(r.Match) == 0 {
				return fmt.Errorf("mcp_policy.%s[%d].match must name at least one pattern", field, i)
			}
		}
	}
	if p.ToolCallsPerMinute < 0 || p.ToolCallsPerMinute > MaxMCPToolsPerMinute {
		return fmt.Errorf("mcp_policy.tool_calls_per_minute must be between 0 and %d", MaxMCPToolsPerMinute)
	}
	if p.MaxBodyBytes < 1 || p.MaxBodyBytes > MaxMCPMaxBody {
		return fmt.Errorf("mcp_policy.max_body_bytes must be between 1 and %d", MaxMCPMaxBody)
	}
	return nil
}
