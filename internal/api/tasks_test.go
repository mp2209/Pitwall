package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
)

// agentCalls runs one simulated agent conversation of n calls against the
// gateway, growing the history each turn, and returns the responses' task IDs.
func agentCalls(t *testing.T, url, key, opening string, n int, headers ...string) []string {
	t.Helper()
	msgs := []map[string]any{{"role": "user", "content": opening}}
	var ids []string
	for i := range n {
		body, _ := json.Marshal(map[string]any{
			"model": "mock-claude", "max_tokens": 200,
			"system":   "You are a careful coding agent. " + strings.Repeat("Follow the repo's conventions. ", 20),
			"messages": msgs,
		})
		resp := post(t, url+"/v1/messages", string(body), append([]string{"x-api-key", key}, headers...)...)
		if resp.StatusCode != 200 {
			t.Fatalf("call %d: status %d", i+1, resp.StatusCode)
		}
		ids = append(ids, resp.Header.Get("x-pitwall-task-id"))
		var out struct {
			Content []struct{ Text string } `json:"content"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": out.Content[0].Text},
			map[string]any{"role": "user", "content": fmt.Sprintf("tool output %d: %s", i, strings.Repeat("log line\n", 40*(i+1)))})
	}
	return ids
}

func TestTaskTracesEndToEnd(t *testing.T) {
	f := newFullStack(t)
	user := f.mustAdmin(t, "POST", "/admin/users", `{"email":"dev@example.test"}`, 201)
	key := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, user["id"]), 201)["key"].(string)
	other := f.mustAdmin(t, "POST", "/admin/users", `{"email":"other@example.test"}`, 201)
	otherKey := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, other["id"]), 201)["key"].(string)

	headerIDs := agentCalls(t, f.url, key, "Fix the failing parser test", 3,
		"x-pitwall-task", "fix-parser-1", "x-pitwall-agent", "test-agent", "x-pitwall-config", "mock/full-history")
	inferredIDs := agentCalls(t, f.url, key, "Add a --verbose flag", 3)
	for _, ids := range [][]string{headerIDs, inferredIDs} {
		if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
			t.Fatalf("a run's calls should share one task ID: %v", ids)
		}
	}
	if !strings.HasPrefix(headerIDs[0], "t_") || !strings.HasPrefix(inferredIDs[0], "ti_") || headerIDs[0] == inferredIDs[0] {
		t.Fatalf("task IDs: header %s inferred %s", headerIDs[0], inferredIDs[0])
	}
	var costs = map[string]metering.Nanos{}
	for range 6 {
		r := f.rec.next(t)
		costs[r.TaskID] += r.Cost
	}

	// Outcomes reported right away, possibly before the calls are written.
	if status, v := f.admin(t, "POST", "/v1/pitwall/tasks/outcome", `{"task":"fix-parser-1","outcome":"solved"}`, key); status != 200 {
		t.Fatalf("header outcome: %d %v", status, v)
	}
	if status, v := f.admin(t, "POST", "/v1/pitwall/tasks/outcome", fmt.Sprintf(`{"task_id":%q,"outcome":"failed"}`, inferredIDs[0]), key); status != 200 {
		t.Fatalf("inferred outcome: %d %v", status, v)
	}
	if status, _ := f.admin(t, "POST", "/v1/pitwall/tasks/outcome", fmt.Sprintf(`{"task_id":%q,"outcome":"solved"}`, inferredIDs[0]), otherKey); status != http.StatusNotFound {
		t.Errorf("another key changed this task's outcome: %d", status)
	}
	if status, _ := f.admin(t, "POST", "/v1/pitwall/tasks/outcome", `{"task":"x","outcome":"great"}`, key); status != http.StatusBadRequest {
		t.Errorf("bad outcome accepted: %d", status)
	}

	var list []any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, _ = f.mustAdmin(t, "GET", "/admin/tasks", "", 200)["data"].([]any)
		if len(list) == 2 && list[0].(map[string]any)["calls"].(float64) == 3 && list[1].(map[string]any)["calls"].(float64) == 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 tasks, got %d: %v", len(list), list)
	}
	byID := map[string]map[string]any{}
	for _, raw := range list {
		task := raw.(map[string]any)
		byID[task["id"].(string)] = task
	}
	h, inf := byID[headerIDs[0]], byID[inferredIDs[0]]
	if h == nil || inf == nil {
		t.Fatalf("tasks missing: %v", byID)
	}
	if h["source"] != "header" || h["external_id"] != "fix-parser-1" || h["agent"] != "test-agent" ||
		h["config_label"] != "mock/full-history" || h["outcome"] != "solved" {
		t.Errorf("header task = %v", h)
	}
	if inf["source"] != "inferred" || inf["outcome"] != "failed" || inf["agent"] != "go-http-client" {
		t.Errorf("inferred task = %v", inf)
	}
	for id, task := range byID {
		if task["calls"].(float64) != 3 || metering.FromUSD(task["cost_usd"].(float64)) != costs[id] {
			t.Errorf("%s: calls %v cost %v, want 3 and %v", id, task["calls"], task["cost_usd"], costs[id].USD())
		}
		first, peak := task["first_context_tokens"].(float64), task["max_context_tokens"].(float64)
		if !(first > 0 && peak > first) {
			t.Errorf("%s: context should grow: first %v max %v", id, first, peak)
		}
		if share := task["repeated_share"].(float64); share <= 0.3 || share >= 1 {
			t.Errorf("%s: repeated share %v, expected most input to be resent", id, share)
		}
	}

	// Per-call view: context grows each turn, the first call repeats
	// nothing, and inference grouped the header run the same way.
	detail := f.mustAdmin(t, "GET", "/admin/tasks/"+headerIDs[0], "", 200)
	calls := detail["calls"].([]any)
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	var lastContext float64
	inferredOfHeaderRun := map[string]bool{}
	for i, raw := range calls {
		c := raw.(map[string]any)
		if ctx := c["context_tokens"].(float64); ctx <= lastContext {
			t.Errorf("call %d: context %v did not grow from %v", i+1, ctx, lastContext)
		} else {
			lastContext = ctx
		}
		if rep := c["repeated_input_tokens"].(float64); (i == 0) != (rep == 0) {
			t.Errorf("call %d: repeated input %v", i+1, rep)
		}
		inferredOfHeaderRun[c["inferred_task_id"].(string)] = true
	}
	if len(inferredOfHeaderRun) != 1 {
		t.Errorf("inference split the header run into %d tasks", len(inferredOfHeaderRun))
	}
	f.mustAdmin(t, "GET", "/admin/tasks/t_doesnotexist", "", http.StatusNotFound)
	if got := f.mustAdmin(t, "GET", "/admin/tasks?config_label=mock/full-history", "", 200)["data"].([]any); len(got) != 1 {
		t.Errorf("config filter returned %d tasks", len(got))
	}
}
