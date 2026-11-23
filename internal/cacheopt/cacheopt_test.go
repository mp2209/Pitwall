package cacheopt

import (
	"encoding/json"
	"strings"
	"testing"
)

var longText = strings.Repeat("You are a careful coding agent working in a Go repository. ", 120) // ~1.7k tokens

func apply(t *testing.T, body, model string) (map[string]any, Result) {
	t.Helper()
	out, res, err := Apply([]byte(body), model)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	return v, res
}

func markers(v map[string]any) int {
	b, _ := json.Marshal(v)
	return strings.Count(string(b), `"cache_control"`)
}

func TestMarksSystemAndNewestTurn(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":` + mustQuote(longText) + `,
		"tools":[{"name":"run_tests","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"Fix the bug"},
			{"role":"assistant","content":[{"type":"text","text":"Running tests"},{"type":"tool_use","id":"t1","name":"run_tests","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"FAIL"}]}],
		"thinking":{"type":"adaptive"},"metadata":{"user_id":"u1"}}`
	v, res := apply(t, body, "claude-sonnet-5-5")
	if res.Added != 2 || res.Outcome != Applied || markers(v) != 2 {
		t.Fatalf("result %+v, markers %d", res, markers(v))
	}
	sys := v["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil || sys[0].(map[string]any)["text"] != longText {
		t.Errorf("system = %v", sys)
	}
	last := v["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if last["type"] != "tool_result" || last["cache_control"] == nil {
		t.Errorf("newest turn block = %v", last)
	}
	if v["thinking"] == nil || v["metadata"] == nil {
		t.Error("fields the optimizer doesn't know must be kept")
	}
}

func TestToolsOnlyAndStringTurn(t *testing.T) {
	tools := `[{"name":"a","description":` + mustQuote(longText) + `,"input_schema":{"type":"object"}},{"name":"b","input_schema":{"type":"object"}}]`
	v, res := apply(t, `{"model":"m","max_tokens":10,"tools":`+tools+`,"messages":[{"role":"user","content":"hi"}]}`, "claude-opus-5-5")
	if res.Added != 2 {
		t.Fatalf("result %+v", res)
	}
	if v["tools"].([]any)[1].(map[string]any)["cache_control"] == nil {
		t.Error("without a system prompt, the last tool carries the marker")
	}
	msg := v["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if msg["text"] != "hi" || msg["cache_control"] == nil {
		t.Errorf("string turn should become a marked block: %v", msg)
	}
}

func TestSkipsIneligibleBlocks(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":` + mustQuote(longText) + `},{"type":"text","text":""}],
		"messages":[{"role":"user","content":"go"},
			{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"thinking","thinking":"hmm","signature":"s"}]},
			{"role":"system","content":"Be brief."}]}`
	v, res := apply(t, body, "claude-opus-5-5")
	if res.Added != 2 {
		t.Fatalf("result %+v", res)
	}
	sys := v["system"].([]any)
	if sys[0].(map[string]any)["cache_control"] == nil || sys[1].(map[string]any)["cache_control"] != nil {
		t.Errorf("empty text blocks can't be marked: %v", sys)
	}
	msgs := v["messages"].([]any)
	asst := msgs[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["cache_control"] == nil || asst[1].(map[string]any)["cache_control"] != nil {
		t.Errorf("thinking blocks can't be marked; the text before it should be: %v", asst)
	}
	if _, isString := msgs[2].(map[string]any)["content"].(string); !isString {
		t.Error("a trailing system message must be left alone")
	}
}

func TestLeavesRequestsAlone(t *testing.T) {
	managed := `{"model":"m","system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	out, res, err := Apply([]byte(managed), "claude-opus-5-5")
	if err != nil || res.Outcome != ClientManaged || string(out) != managed {
		t.Errorf("client markers: %+v %v", res, err)
	}
	short := `{"model":"m","system":"short","messages":[{"role":"user","content":"hi"}]}`
	out, res, _ = Apply([]byte(short), "claude-opus-4-5")
	if res.Outcome != TooShort || res.Added != 0 || string(out) != short {
		t.Errorf("below the minimum: %+v", res)
	}
}

func TestMinCacheTokens(t *testing.T) {
	cases := map[string]int64{
		"claude-sonnet-5-5": 512, "claude-sonnet-5": 1024, "claude-opus-5-5": 512, "claude-fable-5-2": 512,
		"claude-opus-4-5-20251101": 4096, "claude-opus-4-8": 1024, "claude-opus-4": 1024, "claude-haiku-4-5": 4096,
		"claude-opus-4-7": 2048, "mock-claude": DefaultMinimum,
	}
	for model, want := range cases {
		if got := MinCacheTokens(model); got != want {
			t.Errorf("MinCacheTokens(%s) = %d, want %d", model, got, want)
		}
	}
}

func mustQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
