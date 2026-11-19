package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

// agentRun simulates the requests one agent run makes: each call resends
// the history plus the last answer and tool result, with the cache_control
// marker moved to the newest message, as Claude Code does.
type agentRun struct {
	system string
	msgs   []anthropic.Message
}

func newRun(system, task string) *agentRun {
	r := &agentRun{system: system}
	r.msgs = append(r.msgs, anthropic.Message{Role: "user", Content: mustJSON(task)})
	return r
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (r *agentRun) request() *anthropic.MessagesRequest {
	msgs := make([]anthropic.Message, len(r.msgs))
	copy(msgs, r.msgs)
	// Move the breakpoint to the last message, as clients do.
	last := msgs[len(msgs)-1]
	msgs[len(msgs)-1] = anthropic.Message{Role: last.Role, Content: mustJSON([]map[string]any{
		{"type": "text", "text": textOf(last.Content), "cache_control": map[string]string{"type": "ephemeral"}},
	})}
	for i := range len(msgs) - 1 {
		msgs[i] = anthropic.Message{Role: msgs[i].Role, Content: mustJSON([]map[string]any{{"type": "text", "text": textOf(msgs[i].Content)}})}
	}
	return &anthropic.MessagesRequest{
		Model: "m", MaxTokens: 100,
		System:   mustJSON([]map[string]any{{"type": "text", "text": r.system, "cache_control": map[string]string{"type": "ephemeral"}}}),
		Tools:    []anthropic.Tool{{Name: "run_tests", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: msgs,
	}
}

func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func (r *agentRun) turn(answer, toolResult string) {
	r.msgs = append(r.msgs,
		anthropic.Message{Role: "assistant", Content: mustJSON(answer)},
		anthropic.Message{Role: "user", Content: mustJSON(toolResult)})
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestTracker() (*Tracker, *clock) {
	c := &clock{t: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	st := NewMemStore()
	st.now = c.now
	tr := NewTracker(st)
	tr.now = c.now
	return tr, c
}

func assign(t *testing.T, tr *Tracker, key int64, header string, req *anthropic.MessagesRequest) Assignment {
	t.Helper()
	a, err := tr.Assign(context.Background(), Request{KeyID: key, Header: header, Conv: FromAnthropic(req)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMovingCacheControlDoesNotLookLikeAnEdit(t *testing.T) {
	run := newRun("You are a coding agent.", "Fix the failing test")
	first := FromAnthropic(run.request())
	run.turn("Running tests", "FAIL: TestParse")
	second := FromAnthropic(run.request())
	if first.Opening != second.Opening {
		t.Fatal("opening changed between turns")
	}
	if !extends(second.Prefix, first.Prefix) {
		t.Fatal("second call should extend the first despite the moved cache_control marker")
	}
	if got := second.NewMessages(len(first.Prefix)); len(got) != 2 || got[0].Role != "assistant" || got[1].Role != "user" {
		t.Errorf("new messages = %+v, want the answer and the tool result", got)
	}
}

func TestInferenceFollowsOneRun(t *testing.T) {
	tr, clk := newTestTracker()
	run := newRun("You are a coding agent.", "Fix the failing test")
	a1 := assign(t, tr, 1, "", run.request())
	if a1.Source != SourceInferred || a1.CallIndex != 1 || a1.RepeatedBytes != 0 || a1.TaskID != a1.InferredTaskID {
		t.Fatalf("first call = %+v", a1)
	}
	if !a1.TaskStart.Equal(clk.t) {
		t.Errorf("task start = %v, want %v", a1.TaskStart, clk.t)
	}
	taskStart := a1.TaskStart
	prevBytes := a1.TotalBytes
	for i := 2; i <= 6; i++ {
		clk.advance(20 * time.Second)
		run.turn(fmt.Sprintf("step %d", i), strings.Repeat("output ", i*50))
		a := assign(t, tr, 1, "", run.request())
		if a.TaskID != a1.TaskID || a.CallIndex != i || !a.TaskStart.Equal(taskStart) {
			t.Fatalf("call %d: %+v, want task %s", i, a, a1.TaskID)
		}
		// Everything the previous call sent is resent, except the last
		// message, whose cache_control marker moved (its content is equal).
		if a.RepeatedBytes < prevBytes*9/10 || a.RepeatedBytes > a.TotalBytes {
			t.Errorf("call %d: repeated %d of %d bytes; previous call was %d", i, a.RepeatedBytes, a.TotalBytes, prevBytes)
		}
		prevBytes = a.TotalBytes
	}
}

func TestInferenceSeparatesTasksAndRuns(t *testing.T) {
	tr, clk := newTestTracker()
	a := newRun("You are a coding agent.", "Fix test A")
	b := newRun("You are a coding agent.", "Fix test B")
	ta := assign(t, tr, 1, "", a.request()).TaskID
	tb := assign(t, tr, 1, "", b.request()).TaskID
	if ta == tb {
		t.Fatal("different opening turns must be different tasks")
	}
	// Interleaved calls stay with their own runs.
	for range 3 {
		clk.advance(10 * time.Second)
		a.turn("a", "a-out")
		b.turn("b", "b-out")
		if got := assign(t, tr, 1, "", a.request()).TaskID; got != ta {
			t.Fatalf("run A moved to %s", got)
		}
		if got := assign(t, tr, 1, "", b.request()).TaskID; got != tb {
			t.Fatalf("run B moved to %s", got)
		}
	}
	// The same task started again (a benchmark repeat) is a new run.
	clk.advance(10 * time.Second)
	again := assign(t, tr, 1, "", newRun("You are a coding agent.", "Fix test A").request())
	if again.TaskID == ta || again.CallIndex != 1 {
		t.Errorf("a restart should open a new task: %+v", again)
	}
	// A one-call task run twice with the identical request (two Claude Code
	// sessions with the same prompt): the second is a new run, not a
	// continuation that repeats 100% of its input.
	clk.advance(10 * time.Second)
	single := newRun("You are a coding agent.", "say hi")
	s1 := assign(t, tr, 1, "", single.request())
	clk.advance(30 * time.Second)
	s2 := assign(t, tr, 1, "", single.request())
	if s2.TaskID == s1.TaskID || s2.CallIndex != 1 || s2.RepeatedBytes != 0 {
		t.Errorf("identical repeat of a one-call run should be a new task: first %+v second %+v", s1, s2)
	}
	// ...while a longer run still continues normally after it.
	single.turn("hi!", "now fix the bug")
	if s3 := assign(t, tr, 1, "", single.request()); s3.TaskID != s2.TaskID || s3.CallIndex != 2 {
		t.Errorf("the repeat's next call should continue the repeat: %+v", s3)
	}

	// The same opening under another key is another task.
	if other := assign(t, tr, 2, "", a.request()).TaskID; other == ta {
		t.Error("tasks must not cross keys")
	}
}

func TestInferenceIdleGapAndEditedHistory(t *testing.T) {
	tr, clk := newTestTracker()
	run := newRun("sys", "Refactor the parser")
	run.turn("reading", "file contents "+strings.Repeat("x", 2000))
	run.turn("editing", "ok")
	first := assign(t, tr, 1, "", run.request())

	// The harness trims an old tool output: history is edited, not extended.
	run.msgs[2] = anthropic.Message{Role: "user", Content: mustJSON("[output trimmed]")}
	run.turn("testing", "PASS")
	clk.advance(time.Minute)
	trimmed := assign(t, tr, 1, "", run.request())
	if trimmed.TaskID != first.TaskID {
		t.Fatalf("trimmed history should stay in the task: %s vs %s", trimmed.TaskID, first.TaskID)
	}
	// Shared: tools, system, the first user message and the first answer.
	if trimmed.SharedPieces != headerPieces+2 {
		t.Errorf("shared pieces = %d, want %d (up to the trimmed message)", trimmed.SharedPieces, headerPieces+2)
	}

	clk.advance(6 * time.Minute) // past the 5-minute idle gap
	run.turn("more", "more")
	late := assign(t, tr, 1, "", run.request())
	if late.TaskID == first.TaskID || late.CallIndex != 1 {
		t.Errorf("a call after the idle gap should start a new task: %+v", late)
	}
}

func TestHeaderTasks(t *testing.T) {
	tr, clk := newTestTracker()
	run := newRun("sys", "Fix it")
	a1 := assign(t, tr, 1, "run-42", run.request())
	if a1.Source != SourceHeader || a1.ExternalID != "run-42" || a1.TaskID != HeaderTaskID(1, "run-42") || a1.CallIndex != 1 {
		t.Fatalf("header call = %+v", a1)
	}
	if a1.InferredTaskID == "" || a1.InferredTaskID == a1.TaskID {
		t.Errorf("inference should still run alongside the header: %+v", a1)
	}
	clk.advance(10 * time.Minute) // header tasks don't close on idle
	run.turn("x", "y")
	a2 := assign(t, tr, 1, "run-42", run.request())
	if a2.TaskID != a1.TaskID || a2.CallIndex != 2 || a2.RepeatedBytes == 0 {
		t.Errorf("second header call = %+v", a2)
	}
	if HeaderTaskID(1, "run-42") == HeaderTaskID(2, "run-42") {
		t.Error("the same label under two keys must be two tasks")
	}
}

type failingStore struct{}

func (failingStore) Update(context.Context, string, time.Duration, func([]byte) ([]byte, error)) error {
	return errors.New("redis down")
}

func TestStoreFailure(t *testing.T) {
	tr := NewTracker(failingStore{})
	conv := FromAnthropic(newRun("sys", "x").request())
	a, err := tr.Assign(context.Background(), Request{KeyID: 1, Header: "label", Conv: conv})
	if err == nil || a.TaskID != HeaderTaskID(1, "label") {
		t.Errorf("header task should survive a store failure: %+v %v", a, err)
	}
	if a, err := tr.Assign(context.Background(), Request{KeyID: 1, Conv: conv}); err == nil || a.TaskID != "" {
		t.Errorf("without a header there is no task when the store fails: %+v %v", a, err)
	}
}

func TestRedisStoreConcurrentUpdates(t *testing.T) {
	mr := miniredis.RunT(t)
	st := NewRedisStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := st.Update(context.Background(), "k", time.Minute, func(old []byte) ([]byte, error) {
				var n int
				_ = json.Unmarshal(old, &n)
				return json.Marshal(n + 1)
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := mr.Get("k")
	if got != "20" {
		t.Errorf("counter = %s, want 20 (no lost updates)", got)
	}
	if ttl := mr.TTL("k"); ttl <= 0 || ttl > time.Minute {
		t.Errorf("ttl = %v", ttl)
	}
}

func TestFromOpenAI(t *testing.T) {
	sys := json.RawMessage(`"You are an agent"`)
	req := &openai.ChatRequest{Model: "m", Messages: []openai.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: json.RawMessage(`"Fix it"`)},
	}}
	c1 := FromOpenAI(req)
	req.Messages = append(req.Messages,
		openai.Message{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "c1", Type: "function", Function: openai.FunctionCall{Name: "t", Arguments: "{}"}}}},
		openai.Message{Role: "tool", ToolCallID: "c1", Content: json.RawMessage(`"done"`)})
	c2 := FromOpenAI(req)
	if c1.Opening != c2.Opening || !extends(c2.Prefix, c1.Prefix) || c1.Messages != 1 || c2.Messages != 3 {
		t.Errorf("openai conversation: %+v / %+v", c1, c2)
	}
}

func TestLabelsAndAgentNames(t *testing.T) {
	if got := CleanLabel("  fix\x00-bug\n "); got != "fix-bug" {
		t.Errorf("CleanLabel = %q", got)
	}
	if got := CleanLabel(strings.Repeat("é", 150)); len(got) > 200 || !strings.HasPrefix(got, "é") {
		t.Errorf("long label cut badly: %d bytes", len(got))
	}
	cases := map[[2]string]string{
		{"", "claude-cli/2.1.325 (external, cli)"}: "claude-cli",
		{"", "Anthropic/Python 2.8.0"}:             "anthropic",
		{"pitwall-agent", "Go-http-client/1.1"}:    "pitwall-agent",
		{"", ""}:                                   "",
	}
	for in, want := range cases {
		if got := AgentName(in[0], in[1]); got != want {
			t.Errorf("AgentName(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
