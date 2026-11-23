package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
)

const runTestsTool = `[{"name":"run_tests","description":"Run the tests","input_schema":{"type":"object","properties":{}}}]`

// stuckRequest is an agent request whose history repeats the same failing
// test run n times (tool IDs and timings differ, as they do for real).
func stuckRequest(n int) string {
	msgs := []map[string]any{{"role": "user", "content": "Fix the failing test"}}
	for i := range n {
		id := fmt.Sprintf("toolu_%d", i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": id, "name": "run_tests", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id,
				"content": fmt.Sprintf("--- FAIL: TestCount (0.%02ds)\nFAIL wordcount 0.%03ds", i, 100+i)}}})
	}
	b, _ := json.Marshal(map[string]any{"model": "mock-claude", "max_tokens": 100, "tools": json.RawMessage(runTestsTool), "messages": msgs})
	return string(b)
}

func TestGuardWarnsThenStopsAndMarksTheTask(t *testing.T) {
	f := newFullStack(t)
	user := f.mustAdmin(t, "POST", "/admin/users", `{"email":"dev@example.test"}`, 201)
	key := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, user["id"]), 201)["key"].(string)

	var taskID string
	for n := 1; n <= 5; n++ {
		resp := post(t, f.url+"/v1/messages", stuckRequest(n), "x-api-key", key, "x-pitwall-task", "stuck-1")
		body, _ := io.ReadAll(resp.Body)
		warning := resp.Header.Get("x-pitwall-warning")
		taskID = resp.Header.Get("x-pitwall-task-id")
		switch {
		case n < 3:
			if resp.StatusCode != 200 || warning != "" {
				t.Errorf("repeat %d: status %d warning %q, want a quiet 200", n, resp.StatusCode, warning)
			}
		case n < 5:
			if resp.StatusCode != 200 || !strings.HasPrefix(warning, "repeated_exchange=") {
				t.Errorf("repeat %d: status %d warning %q, want a warned 200", n, resp.StatusCode, warning)
			}
		default:
			var e struct {
				Type  string `json:"type"`
				Error struct{ Type, Message string }
			}
			_ = json.Unmarshal(body, &e)
			if resp.StatusCode != http.StatusBadRequest || e.Error.Type != "invalid_request_error" ||
				!strings.Contains(e.Error.Message, "same step 5 times") || !strings.Contains(e.Error.Message, "run_tests {}") {
				t.Errorf("repeat 5: status %d body %s, want a 400 explaining the loop", resp.StatusCode, body)
			}
		}
	}
	recs := map[int]Record{}
	for range 5 {
		r := f.rec.next(t)
		recs[len(recs)+1] = r
	}
	if r := recs[5]; r.GuardAction != "stop" || r.Status != 400 || r.Cost != 0 || r.Reserved != 0 {
		t.Errorf("stopped call record: action %q status %d cost %v reserved %v", r.GuardAction, r.Status, r.Cost, r.Reserved)
	}

	var task map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task = f.mustAdmin(t, "GET", "/admin/tasks/"+taskID, "", 200)["task"].(map[string]any)
		if task["calls"].(float64) == 5 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if task["outcome"] != "stopped" || task["guard_events"].(float64) != 3 || task["failed_calls"].(float64) != 1 {
		t.Errorf("task = outcome %v guard_events %v failed %v, want stopped/3/1", task["outcome"], task["guard_events"], task["failed_calls"])
	}
	calls := f.mustAdmin(t, "GET", "/admin/tasks/"+taskID, "", 200)["calls"].([]any)
	if last := calls[4].(map[string]any); last["guard_action"] != "stop" || last["guard_signal"] != "repeated_exchange:stop" {
		t.Errorf("last call = %v", last)
	}
	// A reported outcome still wins over the guard's.
	if status, _ := f.admin(t, "POST", "/v1/pitwall/tasks/outcome", `{"task":"stuck-1","outcome":"failed"}`, key); status != 200 {
		t.Errorf("outcome report: %d", status)
	}
}

func TestTaskCostCapStopsATask(t *testing.T) {
	// A call here costs about $0.001, so the cap leaves room to warn first.
	f := newFullStackWith(t, "guard: {task_cost_usd: {warn: 0.0015, stop: 0.0035}}")
	user := f.mustAdmin(t, "POST", "/admin/users", `{"email":"dev@example.test"}`, 201)
	key := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, user["id"]), 201)["key"].(string)
	var statuses []string
	var spent metering.Nanos
	for i := range 10 {
		body := fmt.Sprintf(`{"model":"mock-claude","max_tokens":100,"messages":[{"role":"user","content":"task step %d %s"}]}`, i, strings.Repeat("context ", 80))
		resp := post(t, f.url+"/v1/messages", body, "x-api-key", key, "x-pitwall-task", "pricey")
		w := resp.Header.Get("x-pitwall-warning")
		statuses = append(statuses, fmt.Sprintf("%d%s", resp.StatusCode, map[bool]string{true: "w", false: ""}[w != ""]))
		r := f.rec.next(t)
		spent += r.Cost
		if resp.StatusCode == 400 {
			if !strings.Contains(r.Error, "this task has cost $") || spent < metering.FromUSD(0.001) {
				t.Errorf("stop after $%.5f: %q", spent.USD(), r.Error)
			}
			break
		}
	}
	if got := statuses[len(statuses)-1]; got != "400w" {
		t.Errorf("statuses %v: the task should be stopped once its cost passes the cap", statuses)
	}
	if !strings.Contains(strings.Join(statuses, ","), "200w") {
		t.Errorf("statuses %v: a warning should come before the stop", statuses)
	}
}

// The same multi-turn conversation through two keys, one with the cache
// optimizer on: the gateway's breakpoints let later turns read the prefix
// from the (simulated) provider cache, and the task costs less.
func TestCacheOptimizerSavesOnRepeatedTurns(t *testing.T) {
	f := newFullStack(t)
	user := f.mustAdmin(t, "POST", "/admin/users", `{"email":"dev@example.test"}`, 201)
	off := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, user["id"]), 201)["key"].(string)
	on := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v,"cache_optimizer_on":true}`, user["id"]), 201)["key"].(string)

	system := strings.Repeat("Follow the repository's conventions and keep changes small. ", 120) // ~1.8k tokens
	actions := []string{"reading parser.go", "running the tests", "editing lexer.go", "reading the failing test",
		"adding a test case", "running the tests again"}
	run := func(key, label string) (cost metering.Nanos, reads int64, recs []Record) {
		msgs := []map[string]any{{"role": "user", "content": "Fix the parser"}}
		for turn := range 6 {
			body, _ := json.Marshal(map[string]any{"model": "mock-claude", "max_tokens": 100, "system": system, "messages": msgs})
			resp := post(t, f.url+"/v1/messages", string(body), "x-api-key", key, "x-pitwall-task", label)
			if resp.StatusCode != 200 {
				t.Fatalf("%s turn %d: %d", label, turn, resp.StatusCode)
			}
			r := f.rec.next(t)
			recs = append(recs, r)
			cost += r.Cost
			reads += r.Usage.CachedReadTokens
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": "Now " + actions[turn]},
				map[string]any{"role": "user", "content": "Output of " + actions[turn] + ": " + strings.Repeat("log line ", 60)})
		}
		return cost, reads, recs
	}
	costOff, readsOff, recsOff := run(off, "cache-off")
	costOn, readsOn, recsOn := run(on, "cache-on")
	for _, r := range recsOff {
		if r.CacheOptimizer != "off" || r.BreakpointsAdded != 0 {
			t.Fatalf("optimizer-off call: %q %d", r.CacheOptimizer, r.BreakpointsAdded)
		}
	}
	for _, r := range recsOn {
		if r.CacheOptimizer != "applied" || r.BreakpointsAdded != 2 {
			t.Fatalf("optimizer-on call: %q %d", r.CacheOptimizer, r.BreakpointsAdded)
		}
	}
	if readsOff != 0 || readsOn == 0 || costOn >= costOff {
		t.Errorf("off: $%.6f, %d cache reads; on: $%.6f, %d cache reads", costOff.USD(), readsOff, costOn.USD(), readsOn)
	}
	// The first call only writes, so it costs more; the saving comes after.
	if recsOn[0].Cost <= recsOff[0].Cost || recsOn[5].Cost >= recsOff[5].Cost {
		t.Errorf("first call on/off $%.6f/$%.6f, last $%.6f/$%.6f", recsOn[0].Cost.USD(), recsOff[0].Cost.USD(), recsOn[5].Cost.USD(), recsOff[5].Cost.USD())
	}
	t.Logf("6-turn task: optimizer off $%.6f, on $%.6f (%.0f%% saved)", costOff.USD(), costOn.USD(), 100*(1-costOn.USD()/costOff.USD()))

	// A client that places its own markers is left alone.
	managed := `{"model":"mock-claude","max_tokens":10,"system":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	if resp := post(t, f.url+"/v1/messages", managed, "x-api-key", on); resp.StatusCode != 200 {
		t.Fatalf("managed: %d", resp.StatusCode)
	}
	if r := f.rec.next(t); r.CacheOptimizer != "client_managed" || r.BreakpointsAdded != 0 {
		t.Errorf("client-managed call: %q %d", r.CacheOptimizer, r.BreakpointsAdded)
	}
}
