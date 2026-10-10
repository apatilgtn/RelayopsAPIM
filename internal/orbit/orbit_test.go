package orbit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeModel replies with scripted messages and records each request body.
func fakeModel(t *testing.T, replies ...func(body map[string]any) (int, string)) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)
		if i >= len(replies) {
			t.Fatalf("unexpected extra model call %d", i+1)
		}
		status, out := replies[i](body)
		i++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func reply(content string) func(map[string]any) (int, string) {
	return func(map[string]any) (int, string) {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}})
		return 200, string(b)
	}
}

func toolCall(name, args string) func(map[string]any) (int, string) {
	return func(map[string]any) (int, string) {
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
		}}}})
		return 200, string(b)
	}
}

func TestAskRunsToolsThenAnswers(t *testing.T) {
	srv, seen := fakeModel(t, toolCall("traffic", `{"window":"1h"}`), reply("orders had 3 errors."))
	var gotArgs map[string]any
	tools := []Tool{{Name: "traffic", Description: "d", Run: func(_ context.Context, a map[string]any) (string, error) {
		gotArgs = a
		return `{"errors":3}`, nil
	}}}
	ans, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"}).Ask(context.Background(), "sys",
		[]Message{{Role: "user", Content: "errors?"}}, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Text != "orders had 3 errors." || ans.Mode != "tools" || len(ans.Steps) != 1 || !ans.Steps[0].OK || gotArgs["window"] != "1h" {
		t.Fatalf("answer: %+v args=%v", ans, gotArgs)
	}
	// The second call must carry the tool result back to the model.
	msgs := (*seen)[1]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "tool" || last["tool_call_id"] != "c1" || !strings.Contains(last["content"].(string), `"errors":3`) {
		t.Fatalf("tool result not returned to model: %v", last)
	}
}

func TestAskFallsBackToSnapshotWhenToolsUnsupported(t *testing.T) {
	srv, seen := fakeModel(t,
		func(map[string]any) (int, string) { return 400, `{"error":"model does not support tools"}` },
		reply("From the snapshot: all healthy."))
	ans, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"}).Ask(context.Background(), "sys",
		[]Message{{Role: "user", Content: "status?"}}, []Tool{{Name: "x", Run: func(context.Context, map[string]any) (string, error) { return "", nil }}},
		func(context.Context) (string, []Step) {
			return `{"healthy":true}`, []Step{{Tool: "overview", OK: true}}
		})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Mode != "context" || ans.Text != "From the snapshot: all healthy." || len(ans.Steps) != 1 {
		t.Fatalf("answer: %+v", ans)
	}
	if _, hasTools := (*seen)[1]["tools"]; hasTools {
		t.Fatal("fallback call must not offer tools")
	}
	sys := (*seen)[1]["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, `{"healthy":true}`) {
		t.Fatalf("snapshot missing from system prompt: %s", sys)
	}
}

func TestAskReportsToolErrorsAndUnknownTools(t *testing.T) {
	srv, _ := fakeModel(t, toolCall("nope", `{}`), toolCall("boom", `{}`), reply("could not check"))
	tools := []Tool{{Name: "boom", Run: func(context.Context, map[string]any) (string, error) { return "", errors.New("HTTP 403: forbidden") }}}
	ans, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"}).Ask(context.Background(), "sys",
		[]Message{{Role: "user", Content: "?"}}, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Steps) != 2 || ans.Steps[0].Error != "unknown tool" || ans.Steps[1].OK || !strings.Contains(ans.Steps[1].Error, "403") {
		t.Fatalf("steps: %+v", ans.Steps)
	}
}

func TestAskStopsAfterMaxSteps(t *testing.T) {
	loop := toolCall("t", `{}`)
	srv, seen := fakeModel(t, loop, loop, reply("final"))
	tools := []Tool{{Name: "t", Run: func(context.Context, map[string]any) (string, error) { return "{}", nil }}}
	ans, err := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m", MaxSteps: 2}).Ask(context.Background(), "sys",
		[]Message{{Role: "user", Content: "?"}}, tools, nil)
	if err != nil || ans.Text != "final" {
		t.Fatalf("answer %+v err %v", ans, err)
	}
	if _, hasTools := (*seen)[2]["tools"]; hasTools {
		t.Fatal("the closing call must not offer tools")
	}
}

func TestAskWithoutModel(t *testing.T) {
	if _, err := New(Config{}).Ask(context.Background(), "", nil, nil, nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}
