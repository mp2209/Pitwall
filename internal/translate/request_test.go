package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeOpenAI(t *testing.T, s string) *openai.ChatRequest {
	t.Helper()
	var r openai.ChatRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func decodeAnthropic(t *testing.T, s string) *anthropic.MessagesRequest {
	t.Helper()
	var r anthropic.MessagesRequest
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestOpenAIToAnthropicRequestAgentTurn(t *testing.T) {
	in := decodeOpenAI(t, `{
		"model": "alias",
		"max_completion_tokens": 300,
		"stop": "END",
		"parallel_tool_calls": false,
		"user": "dev-1",
		"messages": [
			{"role": "system", "content": "You are a coding agent."},
			{"role": "developer", "content": [{"type": "text", "text": "Be brief."}]},
			{"role": "user", "content": [
				{"type": "text", "text": "What is in this screenshot?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]},
			{"role": "assistant", "content": "Let me look.", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "read", "arguments": "{\"path\":\"a.go\"}"}},
				{"id": "call_2", "type": "function", "function": {"name": "read", "arguments": "not json"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "package a"},
			{"role": "tool", "tool_call_id": "call_2", "content": "package b"},
			{"role": "user", "content": "Now fix it."}
		],
		"tools": [{"type": "function", "function": {"name": "read", "description": "Read a file"}}],
		"tool_choice": "required"
	}`)
	out, err := OpenAIToAnthropicRequest(in, "claude-x", 8192)
	if err != nil {
		t.Fatal(err)
	}
	if out.Model != "claude-x" || out.MaxTokens != 300 || out.Metadata.UserID != "dev-1" {
		t.Errorf("basics wrong: model=%s max=%d meta=%+v", out.Model, out.MaxTokens, out.Metadata)
	}
	if got := strings.Join(out.StopSequences, ","); got != "END" {
		t.Errorf("stop = %q", got)
	}
	if got := string(out.System); got != `[{"type":"text","text":"You are a coding agent."},{"type":"text","text":"Be brief."}]` {
		t.Errorf("system = %s", got)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want user, assistant, user (tool results merged with the next user turn), got %d: %s", len(out.Messages), mustJSON(t, out.Messages))
	}
	if got := string(out.Messages[0].Content); got != `[{"type":"text","text":"What is in this screenshot?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]` {
		t.Errorf("user content = %s", got)
	}
	if got := string(out.Messages[1].Content); got != `[{"type":"text","text":"Let me look."},{"type":"tool_use","id":"call_1","name":"read","input":{"path":"a.go"}},{"type":"tool_use","id":"call_2","name":"read","input":{"arguments":"not json"}}]` {
		t.Errorf("assistant content = %s", got)
	}
	if got := string(out.Messages[2].Content); got != `[{"type":"tool_result","tool_use_id":"call_1","content":"package a"},{"type":"tool_result","tool_use_id":"call_2","content":"package b"},{"type":"text","text":"Now fix it."}]` {
		t.Errorf("merged user content = %s", got)
	}
	if len(out.Tools) != 1 || string(out.Tools[0].InputSchema) != `{"type":"object","properties":{}}` {
		t.Errorf("tools = %s", mustJSON(t, out.Tools))
	}
	if tc := out.ToolChoice; tc == nil || tc.Type != "any" || tc.DisableParallelToolUse == nil || !*tc.DisableParallelToolUse {
		t.Errorf("tool_choice = %s", mustJSON(t, out.ToolChoice))
	}
}

func TestOpenAIToAnthropicRequestDefaults(t *testing.T) {
	out, err := OpenAIToAnthropicRequest(decodeOpenAI(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "m", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens != 4096 {
		t.Errorf("max_tokens default = %d", out.MaxTokens)
	}
	if out.System != nil || out.ToolChoice != nil {
		t.Errorf("unexpected system/tool_choice: %s", mustJSON(t, out))
	}
}

func TestOpenAIToolChoiceForms(t *testing.T) {
	cases := map[string]string{
		`"auto"`: `{"type":"auto"}`,
		`"none"`: `{"type":"none"}`,
		`{"type":"function","function":{"name":"bash"}}`: `{"type":"tool","name":"bash"}`,
	}
	for in, want := range cases {
		got, err := openAIToolChoice(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if g := mustJSON(t, got); g != want {
			t.Errorf("%s -> %s, want %s", in, g, want)
		}
	}
	if _, err := openAIToolChoice(json.RawMessage(`"sometimes"`)); err == nil {
		t.Error("unknown tool_choice should fail")
	}
}

func TestOpenAIToAnthropicRejectsUnknownRole(t *testing.T) {
	_, err := OpenAIToAnthropicRequest(decodeOpenAI(t, `{"model":"m","messages":[{"role":"wizard","content":"x"}]}`), "m", 1)
	if err == nil {
		t.Fatal("want error for unknown role")
	}
}

func TestAnthropicToOpenAIRequestAgentTurn(t *testing.T) {
	in := decodeAnthropic(t, `{
		"model": "alias",
		"max_tokens": 1000,
		"stream": true,
		"stop_sequences": ["END"],
		"metadata": {"user_id": "dev-1"},
		"system": [{"type": "text", "text": "You are a coding agent.", "cache_control": {"type": "ephemeral"}}],
		"messages": [
			{"role": "user", "content": "Fix the test"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "hmm", "signature": "sig"},
				{"type": "text", "text": "Running tests."},
				{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": {"command": "go test"}}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "Here you go"},
				{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": true, "content": [{"type": "text", "text": "FAIL"}]}
			]}
		],
		"tools": [
			{"name": "bash", "description": "Run a command", "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}}},
			{"type": "web_search_20250305", "name": "web_search"}
		],
		"tool_choice": {"type": "any", "disable_parallel_tool_use": true}
	}`)
	out, err := AnthropicToOpenAIRequest(in, "gpt-x", "max_completion_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != 1000 || out.MaxTokens != nil {
		t.Errorf("max tokens param wrong: %s", mustJSON(t, out))
	}
	if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Error("stream should ask for usage")
	}
	if out.User != "dev-1" || string(out.Stop) != `["END"]` {
		t.Errorf("user/stop wrong: %s", mustJSON(t, out))
	}
	got := mustJSON(t, out.Messages)
	want := `[{"role":"system","content":"You are a coding agent."},` +
		`{"role":"user","content":"Fix the test"},` +
		`{"role":"assistant","content":"Running tests.","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"bash","arguments":"{\"command\": \"go test\"}"}}]},` +
		`{"role":"tool","content":"Error: FAIL","tool_call_id":"toolu_1"},` +
		`{"role":"user","content":"Here you go"}]`
	if got != want {
		t.Errorf("messages:\n got %s\nwant %s", got, want)
	}
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "bash" {
		t.Errorf("server tool should be dropped: %s", mustJSON(t, out.Tools))
	}
	if string(out.ToolChoice) != `"required"` || out.ParallelToolCalls == nil || *out.ParallelToolCalls {
		t.Errorf("tool choice = %s parallel = %v", out.ToolChoice, out.ParallelToolCalls)
	}
}

// Claude Code sends system-role turns inside messages.
func TestAnthropicSystemMessageInConversation(t *testing.T) {
	in := decodeAnthropic(t, `{"model":"m","max_tokens":5,"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":[{"type":"text","text":"Reminder: be brief."}]},
		{"role":"user","content":"go"}
	]}`)
	out, err := AnthropicToOpenAIRequest(in, "m", "max_tokens")
	if err != nil {
		t.Fatal(err)
	}
	got := mustJSON(t, out.Messages)
	want := `[{"role":"user","content":"hi"},{"role":"system","content":"Reminder: be brief."},{"role":"user","content":"go"}]`
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestAnthropicImageBecomesDataURL(t *testing.T) {
	in := decodeAnthropic(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
	]}]}`)
	out, err := AnthropicToOpenAIRequest(in, "m", "max_tokens")
	if err != nil {
		t.Fatal(err)
	}
	got := string(out.Messages[0].Content)
	want := `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,QUJD"}}]`
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 1 {
		t.Error("max_tokens param should be used")
	}
}
