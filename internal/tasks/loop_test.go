package tasks

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

// history builds an Anthropic conversation from (tool call, result) steps.
func history(steps ...[2]string) *anthropic.MessagesRequest {
	msgs := []anthropic.Message{{Role: "user", Content: mustJSON("Fix the failing test")}}
	for i, s := range steps {
		id := fmt.Sprintf("toolu_%d", i) // IDs always differ between turns
		msgs = append(msgs,
			anthropic.Message{Role: "assistant", Content: json.RawMessage(fmt.Sprintf(
				`[{"type":"thinking","thinking":"attempt %d","signature":"s%d"},{"type":"tool_use","id":%q,"name":"run","input":%s}]`, i, i, id, s[0]))},
			anthropic.Message{Role: "user", Content: json.RawMessage(fmt.Sprintf(
				`[{"type":"tool_result","tool_use_id":%q,"content":%s}]`, id, mustJSON(s[1])))})
	}
	return &anthropic.MessagesRequest{Model: "m", MaxTokens: 10, Messages: msgs}
}

func loopOf(req *anthropic.MessagesRequest) LoopStats { return FromAnthropic(req).Loop }

func TestStuckAgentIsSeen(t *testing.T) {
	// The same failing command, five times; only the timing differs.
	var steps [][2]string
	for i := range 5 {
		steps = append(steps, [2]string{`{"cmd":"go test ./..."}`, fmt.Sprintf("FAIL wordcount 0.%03ds", 100+i)})
	}
	st := loopOf(history(steps...))
	if st.RepeatedExchanges != 5 || st.RepeatedToolCalls != 5 || st.LastToolCall != `run {"cmd":"go test ./..."}` {
		t.Errorf("stuck agent: %+v", st)
	}
}

// Test runners often print the same failures in a different order each run;
// that is still the same result.
func TestReorderedOutputIsTheSameResult(t *testing.T) {
	orders := []string{
		"--- FAIL: TestCount\nCount(a) = 1\nCount(b) = 2\nFAIL 0.10s",
		"--- FAIL: TestCount\nCount(b) = 2\nCount(a) = 1\nFAIL 0.12s",
		"--- FAIL: TestCount\nCount(a) = 1\nCount(b) = 2\nFAIL 0.09s",
		"--- FAIL: TestCount\nCount(b) = 2\nCount(a) = 1\nFAIL 0.11s",
		"--- FAIL: TestCount\nCount(a) = 1\nCount(b) = 2\nFAIL 0.10s",
	}
	var steps [][2]string
	for _, o := range orders {
		steps = append(steps, [2]string{`{}`, o})
	}
	if st := loopOf(history(steps...)); st.RepeatedExchanges != 5 {
		t.Errorf("reordered failure lines should count as the same result: %+v", st)
	}
}

func TestProgressBreaksTheRun(t *testing.T) {
	st := loopOf(history(
		[2]string{`{"cmd":"go test ./..."}`, "FAIL: TestCount"},
		[2]string{`{"cmd":"go test ./..."}`, "FAIL: TestCount"},
		[2]string{`{"cmd":"sed -i s/a/b/ wc.go"}`, "ok"}, // an edit in between
		[2]string{`{"cmd":"go test ./..."}`, "FAIL: TestCount"},
	))
	if st.RepeatedExchanges != 1 || st.RepeatedToolCalls != 1 {
		t.Errorf("an edit between test runs is progress, not a loop: %+v", st)
	}
	// Polling: the same call, results changing.
	st = loopOf(history(
		[2]string{`{"cmd":"gh pr checks"}`, "queued"},
		[2]string{`{"cmd":"gh pr checks"}`, "running"},
		[2]string{`{"cmd":"gh pr checks"}`, "running tests"},
	))
	if st.RepeatedExchanges != 1 || st.RepeatedToolCalls != 3 {
		t.Errorf("polling repeats the call but not the exchange: %+v", st)
	}
}

func TestArgumentOrderDoesNotMatter(t *testing.T) {
	st := loopOf(history(
		[2]string{`{"a":1,"b":2}`, "same"},
		[2]string{`{"b":2, "a":1}`, "same"},
	))
	if st.RepeatedExchanges != 2 || st.RepeatedToolCalls != 2 {
		t.Errorf("key order and spacing must not matter: %+v", st)
	}
}

func TestOpenAILoop(t *testing.T) {
	msgs := []openai.Message{{Role: "system", Content: mustJSON("sys")}, {Role: "user", Content: mustJSON("go")}}
	for i := range 4 {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			openai.Message{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: id, Type: "function",
				Function: openai.FunctionCall{Name: "run_tests", Arguments: "{}"}}}},
			openai.Message{Role: "tool", ToolCallID: id, Content: mustJSON(fmt.Sprintf("FAIL in %dms", 10+i))})
	}
	st := FromOpenAI(&openai.ChatRequest{Model: "m", Messages: msgs}).Loop
	if st.RepeatedExchanges != 4 || st.RepeatedToolCalls != 4 || st.LastToolCall != "run_tests {}" {
		t.Errorf("openai loop: %+v", st)
	}
}

func TestNoToolsNoLoop(t *testing.T) {
	st := loopOf(&anthropic.MessagesRequest{Model: "m", Messages: []anthropic.Message{{Role: "user", Content: mustJSON("hi")}}})
	if st.RepeatedExchanges != 0 || st.RepeatedToolCalls != 0 {
		t.Errorf("a first call has no exchanges: %+v", st)
	}
}
