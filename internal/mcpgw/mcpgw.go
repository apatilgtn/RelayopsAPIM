// Package mcpgw implements the gateway side of the Model Context Protocol
// (Streamable HTTP transport): parsing JSON-RPC requests, deciding which tools
// a caller may use, pinning tool definitions, and filtering tools/list results.
package mcpgw

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/relayops/apim/internal/store"
)

// JSON-RPC error codes the gateway returns. -32001..-32003 are in the
// implementation-defined server error range.
const (
	CodeParseError      = -32700
	CodeInvalidRequest  = -32600
	CodeToolNotAllowed  = -32001
	CodeToolRateLimited = -32002
	CodeToolNotPinned   = -32003
	CodeInvalidParams   = -32602
)

// Message is one JSON-RPC 2.0 message.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// IDKey is a comparable form of a message ID ("" for notifications).
func (m Message) IDKey() string { return string(bytes.TrimSpace(m.ID)) }

// IsRequest reports whether m is a request or notification (has a method).
func (m Message) IsRequest() bool { return m.Method != "" }

// ParseMessages decodes a single message or a batch.
func ParseMessages(body []byte) (msgs []Message, batch bool, err error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, false, errors.New("empty JSON-RPC body")
	}
	if body[0] == '[' {
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, true, fmt.Errorf("invalid JSON-RPC batch: %w", err)
		}
		if len(msgs) == 0 {
			return nil, true, errors.New("empty JSON-RPC batch")
		}
		return msgs, true, nil
	}
	var m Message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, false, fmt.Errorf("invalid JSON-RPC message: %w", err)
	}
	return []Message{m}, false, nil
}

// ToolCall is the params of a tools/call request.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolCall decodes a tools/call request's params.
func (m Message) ToolCall() (ToolCall, error) {
	var c ToolCall
	if err := json.Unmarshal(m.Params, &c); err != nil || c.Name == "" {
		return c, errors.New("tools/call params must include a tool name")
	}
	return c, nil
}

// ErrorResponse encodes a JSON-RPC error response.
func ErrorResponse(id json.RawMessage, code int, msg, reason string) Message {
	errObj, _ := json.Marshal(map[string]any{"code": code, "message": msg, "data": map[string]string{"reason": reason}})
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return Message{JSONRPC: "2.0", ID: id, Error: errObj}
}

// Encode writes messages back as a single message or a batch.
func Encode(msgs []Message, batch bool) []byte {
	if !batch && len(msgs) == 1 {
		b, _ := json.Marshal(msgs[0])
		return b
	}
	b, _ := json.Marshal(msgs)
	return b
}

// Caller identifies who is calling a tool.
type Caller struct {
	ConsumerID string
	PlanName   string
}

// Decision is the outcome for one tool.
type Decision struct {
	Allowed bool
	Code    int
	Reason  string
	Message string
}

// Authorize decides whether caller may use tool under the API's policy.
// Pinning is checked first: with pins configured, only pinned tools exist.
// Then the first matching rule wins; with no match the default applies.
func Authorize(p store.MCPPolicy, c Caller, tool string) Decision {
	if len(p.PinnedTools) > 0 {
		if _, ok := p.PinnedTools[tool]; !ok {
			return Decision{Code: CodeToolNotPinned, Reason: "tool_not_pinned",
				Message: fmt.Sprintf("tool %q is not in this API's approved tool catalog", tool)}
		}
	}
	for _, r := range p.Rules {
		if !matchAny(r.Tools, tool) {
			continue
		}
		if len(r.Consumers) > 0 && !slices.Contains(r.Consumers, c.ConsumerID) {
			continue
		}
		if len(r.Plans) > 0 && !slices.ContainsFunc(r.Plans, func(p string) bool { return strings.EqualFold(p, c.PlanName) }) {
			continue
		}
		if r.Action == "deny" {
			return Decision{Code: CodeToolNotAllowed, Reason: "tool_not_allowed", Message: fmt.Sprintf("tool %q is not allowed for this caller", tool)}
		}
		return Decision{Allowed: true}
	}
	if p.DefaultAction == "deny" {
		return Decision{Code: CodeToolNotAllowed, Reason: "tool_not_allowed", Message: fmt.Sprintf("tool %q is not allowed for this caller", tool)}
	}
	return Decision{Allowed: true}
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// fingerprintFields are the parts of a definition a model reads; a change
// to any of them changes what the tool or prompt claims to do.
var fingerprintFields = map[string][]string{
	KindTool:   {"name", "title", "description", "inputSchema", "outputSchema", "annotations"},
	KindPrompt: {"name", "title", "description", "arguments"},
}

const (
	KindTool     = "tool"
	KindPrompt   = "prompt"
	KindResource = "resource"
)

// Fingerprint returns a tool's name and a hash of its definition.
func Fingerprint(tool json.RawMessage) (name, fp string, err error) {
	return FingerprintOf(KindTool, tool)
}

// FingerprintOf returns a tool's or prompt's name and a hash of its
// definition.
func FingerprintOf(kind string, def json.RawMessage) (name, fp string, err error) {
	var m map[string]any
	if err := json.Unmarshal(def, &m); err != nil {
		return "", "", err
	}
	name, _ = m["name"].(string)
	if name == "" {
		return "", "", fmt.Errorf("%s without a name", kind)
	}
	subset := map[string]any{}
	for _, f := range fingerprintFields[kind] {
		if v, ok := m[f]; ok {
			subset[f] = v
		}
	}
	canon, _ := json.Marshal(subset) // map keys are sorted, recursively
	sum := sha256.Sum256(canon)
	return name, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ItemStatus is one entry of a list result as the gateway judged it.
type ItemStatus struct {
	Kind        string          `json:"kind"`
	Name        string          `json:"name"` // tool or prompt name, or resource URI
	Fingerprint string          `json:"fingerprint,omitempty"`
	// Status: allowed, hidden (policy), unpinned (not in the catalog),
	// changed (definition differs from its pin).
	Status     string          `json:"status"`
	Definition json.RawMessage `json:"-"`
}

// ToolStatus is kept for callers that only deal with tools.
type ToolStatus = ItemStatus

// listShape says where a list result keeps its items and their names.
var listShape = map[string]struct{ key, nameField, kind string }{
	"tools/list":               {"tools", "name", KindTool},
	"prompts/list":             {"prompts", "name", KindPrompt},
	"resources/list":           {"resources", "uri", KindResource},
	"resources/templates/list": {"resourceTemplates", "uriTemplate", KindResource},
}

// IsListMethod reports whether the gateway filters this method's result.
func IsListMethod(method string) bool { _, ok := listShape[method]; return ok }

// FilterToolsList filters a tools/list result.
func FilterToolsList(result json.RawMessage, p store.MCPPolicy, c Caller) (json.RawMessage, []ToolStatus, error) {
	return FilterList("tools/list", result, p, c)
}

// FilterList removes what the caller may not see from a list result:
// unpinned or changed tools and prompts when pins are set, and items the
// rules deny. It returns the rewritten result and every item's status.
func FilterList(method string, result json.RawMessage, p store.MCPPolicy, c Caller) (json.RawMessage, []ItemStatus, error) {
	shape, ok := listShape[method]
	if !ok {
		return result, nil, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(result, &obj); err != nil {
		return result, nil, err
	}
	var items []json.RawMessage
	if raw, ok := obj[shape.key]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return result, nil, err
		}
	}
	pins := map[string]string(nil)
	switch shape.kind {
	case KindTool:
		pins = p.PinnedTools
	case KindPrompt:
		pins = p.PinnedPrompts
	}
	kept := make([]json.RawMessage, 0, len(items))
	statuses := make([]ItemStatus, 0, len(items))
	for _, it := range items {
		st := ItemStatus{Kind: shape.kind, Status: "allowed", Definition: it}
		if shape.kind == KindResource {
			var m map[string]any
			_ = json.Unmarshal(it, &m)
			st.Name, _ = m[shape.nameField].(string)
		} else {
			name, fp, err := FingerprintOf(shape.kind, it)
			if err != nil {
				statuses = append(statuses, ItemStatus{Kind: shape.kind, Status: "invalid"})
				continue
			}
			st.Name, st.Fingerprint = name, fp
			if len(pins) > 0 {
				switch pin, ok := pins[name]; {
				case !ok:
					st.Status = "unpinned"
				case pin != fp:
					st.Status = "changed"
				}
			}
		}
		if st.Status == "allowed" && !AuthorizeKind(p, c, shape.kind, st.Name).Allowed {
			st.Status = "hidden"
		}
		statuses = append(statuses, st)
		if st.Status == "allowed" {
			kept = append(kept, it)
		}
	}
	obj[shape.key], _ = json.Marshal(kept)
	out, err := json.Marshal(obj)
	return out, statuses, err
}

// AuthorizeKind decides access to a tool, a prompt (by name) or a resource
// (by URI).
func AuthorizeKind(p store.MCPPolicy, c Caller, kind, name string) Decision {
	switch kind {
	case KindTool:
		return Authorize(p, c, name)
	case KindPrompt:
		if len(p.PinnedPrompts) > 0 {
			if _, ok := p.PinnedPrompts[name]; !ok {
				return Decision{Code: CodeToolNotPinned, Reason: "prompt_not_pinned",
					Message: fmt.Sprintf("prompt %q is not in this API's approved catalog", name)}
			}
		}
		return matchRules(p.PromptRules, p.DefaultAction, c, name, "prompt")
	default:
		return matchRules(p.ResourceRules, p.DefaultAction, c, name, "resource")
	}
}

func matchRules(rules []store.MCPRule, def string, c Caller, name, what string) Decision {
	deny := Decision{Code: CodeToolNotAllowed, Reason: what + "_not_allowed", Message: fmt.Sprintf("%s %q is not allowed for this caller", what, name)}
	for _, r := range rules {
		if !slices.ContainsFunc(r.Match, func(pat string) bool { return wildcard(pat, name) }) {
			continue
		}
		if len(r.Consumers) > 0 && !slices.Contains(r.Consumers, c.ConsumerID) {
			continue
		}
		if len(r.Plans) > 0 && !slices.ContainsFunc(r.Plans, func(p string) bool { return strings.EqualFold(p, c.PlanName) }) {
			continue
		}
		if r.Action == "deny" {
			return deny
		}
		return Decision{Allowed: true}
	}
	if def == "deny" {
		return deny
	}
	return Decision{Allowed: true}
}

// wildcard matches s against a pattern where "*" matches any run of
// characters (including "/", as URIs need) and "?" one character.
func wildcard(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if wildcard(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
		}
		pattern, s = pattern[1:], s[1:]
	}
	return s == ""
}
