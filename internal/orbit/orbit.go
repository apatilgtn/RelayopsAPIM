// Package orbit is the model side of Orbit AI, the operations assistant built
// into the RelayOps console. It talks to any OpenAI-compatible chat completions
// endpoint (OpenAI, NVIDIA NIM, Ollama, vLLM, or a RelayOps AI route) and runs
// a bounded tool-calling loop. Tools are supplied by the caller; this package
// never decides what data a user may see.
package orbit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config selects the model Orbit uses.
type Config struct {
	BaseURL  string        // e.g. https://integrate.api.nvidia.com/v1 (".../chat/completions" is appended)
	APIKey   string        // sent as "Authorization: Bearer <key>"
	Model    string        // e.g. meta/llama-3.3-70b-instruct
	Timeout  time.Duration // per model call; default 60s
	MaxSteps int           // tool-calling rounds per question; default 6
}

// Enabled reports whether a model is configured.
func (c Config) Enabled() bool { return c.BaseURL != "" && c.Model != "" }

// Message is one chat message in OpenAI wire format.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool is a read-only capability offered to the model.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema for the arguments
	Run         func(ctx context.Context, args map[string]any) (string, error)
}

// Step records one tool the model consulted, shown to the user as evidence.
type Step struct {
	Tool  string         `json:"tool"`
	Args  map[string]any `json:"args,omitempty"`
	OK    bool           `json:"ok"`
	Error string         `json:"error,omitempty"`
}

// Usage is the token count reported by the model.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Answer is Orbit's reply to one question.
type Answer struct {
	Text  string `json:"text"`
	Steps []Step `json:"steps"`
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
	// Mode is "tools" when the model called tools itself, or "context" when
	// the model does not support tool calling and answered from a snapshot.
	Mode string `json:"mode"`
}

// ErrNotConfigured is returned when no model is configured.
var ErrNotConfigured = errors.New("orbit: no model configured")

// maxToolOutput bounds what one tool result adds to the conversation.
const maxToolOutput = 8000

// Client calls the configured model.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a client for cfg.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 6
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout}}
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

// Ask answers the conversation. system is the system prompt; history ends
// with the user's question. snapshot is called only when the model rejects
// tool calling, and returns gateway data to answer from instead.
func (c *Client) Ask(ctx context.Context, system string, history []Message, tools []Tool, snapshot func(context.Context) (string, []Step)) (Answer, error) {
	if c == nil || !c.cfg.Enabled() {
		return Answer{}, ErrNotConfigured
	}
	byName := map[string]Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	msgs := append([]Message{{Role: "system", Content: system}}, history...)
	ans := Answer{Model: c.cfg.Model, Mode: "tools"}

	for step := 0; step < c.cfg.MaxSteps; step++ {
		reply, usage, err := c.complete(ctx, msgs, tools)
		var unsupported *toolsUnsupportedError
		if errors.As(err, &unsupported) && snapshot != nil {
			return c.askWithSnapshot(ctx, system, history, snapshot, ans)
		}
		if err != nil {
			return ans, err
		}
		ans.Usage.PromptTokens += usage.PromptTokens
		ans.Usage.CompletionTokens += usage.CompletionTokens
		if len(reply.ToolCalls) == 0 {
			ans.Text = strings.TrimSpace(reply.Content)
			return ans, nil
		}
		msgs = append(msgs, Message{Role: "assistant", Content: reply.Content, ToolCalls: reply.ToolCalls})
		for i, call := range reply.ToolCalls {
			if i >= 4 { // bound the work one model turn can request
				msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Content: "skipped: at most 4 tools per step"})
				continue
			}
			out, st := runTool(ctx, byName, call)
			ans.Steps = append(ans.Steps, st)
			msgs = append(msgs, Message{Role: "tool", ToolCallID: call.ID, Content: out})
		}
	}
	// Out of steps: ask for an answer from what has been gathered.
	msgs = append(msgs, Message{Role: "user", Content: "Answer now from the data above without calling more tools."})
	reply, usage, err := c.complete(ctx, msgs, nil)
	if err != nil {
		return ans, err
	}
	ans.Usage.PromptTokens += usage.PromptTokens
	ans.Usage.CompletionTokens += usage.CompletionTokens
	ans.Text = strings.TrimSpace(reply.Content)
	return ans, nil
}

func (c *Client) askWithSnapshot(ctx context.Context, system string, history []Message, snapshot func(context.Context) (string, []Step), ans Answer) (Answer, error) {
	data, steps := snapshot(ctx)
	ans.Mode, ans.Steps = "context", steps
	sys := system + "\n\nYou cannot call tools with this model. Answer only from this snapshot of gateway data (JSON); say so if it does not contain the answer:\n" + data
	msgs := append([]Message{{Role: "system", Content: sys}}, history...)
	reply, usage, err := c.complete(ctx, msgs, nil)
	if err != nil {
		return ans, err
	}
	ans.Usage = usage
	ans.Text = strings.TrimSpace(reply.Content)
	return ans, nil
}

func runTool(ctx context.Context, byName map[string]Tool, call ToolCall) (string, Step) {
	st := Step{Tool: call.Function.Name}
	t, ok := byName[call.Function.Name]
	if !ok {
		st.Error = "unknown tool"
		return "error: unknown tool " + call.Function.Name, st
	}
	args := map[string]any{}
	if strings.TrimSpace(call.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			st.Error = "invalid arguments"
			return "error: arguments must be a JSON object", st
		}
	}
	st.Args = args
	out, err := t.Run(ctx, args)
	if err != nil {
		st.Error = err.Error()
		return "error: " + err.Error(), st
	}
	st.OK = true
	if len(out) > maxToolOutput {
		out = out[:maxToolOutput] + "\n…(truncated)"
	}
	return out, st
}

type toolsUnsupportedError struct{ msg string }

func (e *toolsUnsupportedError) Error() string { return e.msg }

type completionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

func (c *Client) complete(ctx context.Context, msgs []Message, tools []Tool) (Message, Usage, error) {
	body := map[string]any{
		"model":       c.cfg.Model,
		"messages":    msgs,
		"temperature": 0.2,
		"max_tokens":  1200,
	}
	if len(tools) > 0 {
		defs := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			params := t.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params,
			}})
		}
		body["tools"] = defs
		body["tool_choice"] = "auto"
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Message{}, Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Message{}, Usage{}, fmt.Errorf("orbit: model unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		detail := strings.TrimSpace(string(data))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		// Models without function calling reject the "tools" field with a 400.
		if len(tools) > 0 && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) &&
			strings.Contains(strings.ToLower(detail), "tool") {
			return Message{}, Usage{}, &toolsUnsupportedError{msg: detail}
		}
		return Message{}, Usage{}, fmt.Errorf("orbit: model returned HTTP %d: %s", resp.StatusCode, detail)
	}
	var out completionResponse
	if err := json.Unmarshal(data, &out); err != nil || len(out.Choices) == 0 {
		return Message{}, Usage{}, fmt.Errorf("orbit: unexpected model response")
	}
	return out.Choices[0].Message, out.Usage, nil
}
