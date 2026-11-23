package mockprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(t *testing.T, url, body string) map[string]any {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAnthropicCacheSimulation(t *testing.T) {
	ts := httptest.NewServer(New(Config{MinCacheTokens: 10}).Handler())
	defer ts.Close()
	system := strings.Repeat("You are a careful coding agent. ", 20)
	body := func(turn string) string {
		return fmt.Sprintf(`{"model":"m","max_tokens":10,
			"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}],
			"messages":[{"role":"user","content":%q}]}`, system, turn)
	}
	usage := func(v map[string]any) (read, write float64) {
		u := v["usage"].(map[string]any)
		return u["cache_read_input_tokens"].(float64), u["cache_creation_input_tokens"].(float64)
	}
	read, write := usage(call(t, ts.URL+"/v1/messages", body("first")))
	if read != 0 || write == 0 {
		t.Errorf("first call: read=%v write=%v, want a cache write", read, write)
	}
	read2, write2 := usage(call(t, ts.URL+"/v1/messages", body("second")))
	if read2 != write || write2 != 0 {
		t.Errorf("second call: read=%v write=%v, want to read the %v tokens written", read2, write2, write)
	}
	// Without cache_control nothing is cached.
	v := call(t, ts.URL+"/v1/messages", fmt.Sprintf(`{"model":"m","max_tokens":10,"system":%q,"messages":[{"role":"user","content":"x"}]}`, system))
	if r, w := usage(v); r != 0 || w != 0 {
		t.Errorf("unmarked prompt: read=%v write=%v", r, w)
	}
}

func TestOpenAIAutomaticPrefixCache(t *testing.T) {
	ts := httptest.NewServer(New(Config{MinCacheTokens: 128}).Handler())
	defer ts.Close()
	long := strings.Repeat("context ", 400)
	first := fmt.Sprintf(`{"model":"m","messages":[{"role":"system","content":%q},{"role":"user","content":"a"}]}`, long)
	second := fmt.Sprintf(`{"model":"m","messages":[{"role":"system","content":%q},{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`, long)
	cached := func(v map[string]any) float64 {
		return v["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)["cached_tokens"].(float64)
	}
	if c := cached(call(t, ts.URL+"/v1/chat/completions", first)); c != 0 {
		t.Errorf("first call cached %v", c)
	}
	c := cached(call(t, ts.URL+"/v1/chat/completions", second))
	if c == 0 || int(c)%128 != 0 {
		t.Errorf("second call cached %v, want a positive multiple of 128", c)
	}
}

func TestLoopModeRepeatsSameToolCall(t *testing.T) {
	ts := httptest.NewServer(New(Config{}).Handler())
	defer ts.Close()
	body := `{"model":"m","messages":[{"role":"user","content":"[[mock:mode=loop]] go"},{"role":"tool","tool_call_id":"x","content":"failed"}],
		"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]}`
	var args []string
	for i := 0; i < 3; i++ {
		v := call(t, ts.URL+"/v1/chat/completions", body)
		tc := v["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
		args = append(args, tc["function"].(map[string]any)["arguments"].(string))
	}
	if args[0] != `{"cmd":"mock"}` || args[0] != args[1] || args[1] != args[2] {
		t.Errorf("loop mode args = %v", args)
	}
}

func TestAnthropicRequiresMaxTokens(t *testing.T) {
	ts := httptest.NewServer(New(Config{}).Handler())
	defer ts.Close()
	v := call(t, ts.URL+"/v1/messages", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if v["type"] != "error" {
		t.Errorf("want error, got %v", v)
	}
}

func TestWordsRejoin(t *testing.T) {
	text := "This is a mock reply."
	if got := strings.Join(words(text), ""); got != text {
		t.Errorf("words do not rejoin: %q", got)
	}
}

// anthropicTurns builds a request with the given messages, a long system
// prompt, and a cache_control marker on the last block of message `mark`
// (-1 for none).
func anthropicTurns(system string, msgs []string, mark int) string {
	var parts []string
	for i, text := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		cc := ""
		if i == mark {
			cc = `,"cache_control":{"type":"ephemeral"}`
		}
		parts = append(parts, fmt.Sprintf(`{"role":%q,"content":[{"type":"text","text":%q%s}]}`, role, text, cc))
	}
	return fmt.Sprintf(`{"model":"m","max_tokens":10,"system":%q,"messages":[%s]}`, system, strings.Join(parts, ","))
}

func cacheUsage(v map[string]any) (read, write float64) {
	u := v["usage"].(map[string]any)
	return u["cache_read_input_tokens"].(float64), u["cache_creation_input_tokens"].(float64)
}

// In an agent loop the client moves the marker to the newest turn each call.
// The previous entry is found by looking back from the new marker, even
// though the old marker is gone, so the whole previous prompt is a hit.
func TestMovingMarkerHitsThroughLookback(t *testing.T) {
	ts := httptest.NewServer(New(Config{MinCacheTokens: 50}).Handler())
	defer ts.Close()
	sys := strings.Repeat("Rules of this repository. ", 40)
	msgs := []string{"Fix the bug", "Running tests", strings.Repeat("FAIL output ", 30)}
	r1, w1 := cacheUsage(call(t, ts.URL+"/v1/messages", anthropicTurns(sys, msgs, 2)))
	if r1 != 0 || w1 == 0 {
		t.Fatalf("first call: read %v write %v", r1, w1)
	}
	msgs = append(msgs, "Editing parser.go", strings.Repeat("ok ", 30))
	r2, w2 := cacheUsage(call(t, ts.URL+"/v1/messages", anthropicTurns(sys, msgs, 4)))
	if r2 != w1 || w2 == 0 {
		t.Errorf("second call: read %v (want the %v written), write %v", r2, w1, w2)
	}
}

func TestLookbackIsLimitedToTwentyPositions(t *testing.T) {
	ts := httptest.NewServer(New(Config{MinCacheTokens: 10}).Handler())
	defer ts.Close()
	sys := strings.Repeat("context ", 50)
	msgs := []string{"start"}
	call(t, ts.URL+"/v1/messages", anthropicTurns(sys, msgs, 0))
	for i := range 24 { // 24 new positions before the next marker
		msgs = append(msgs, fmt.Sprintf("step %d", i))
	}
	if r, _ := cacheUsage(call(t, ts.URL+"/v1/messages", anthropicTurns(sys, msgs, len(msgs)-1))); r != 0 {
		t.Errorf("an entry more than 20 positions back should be missed, read %v", r)
	}
}

func TestTurnsModeCyclesToolsThenAnswers(t *testing.T) {
	ts := httptest.NewServer(New(Config{}).Handler())
	defer ts.Close()
	tools := `"tools":[{"name":"a","input_schema":{"type":"object"}},{"name":"b","input_schema":{"type":"object"}}]`
	msgs := `{"role":"user","content":"[[mock:turns=3]] go"}`
	var names []string
	for range 4 {
		v := call(t, ts.URL+"/v1/messages", `{"model":"m","max_tokens":10,`+tools+`,"messages":[`+msgs+`]}`)
		block := v["content"].([]any)[0].(map[string]any)
		if block["type"] != "tool_use" {
			names = append(names, "text")
			break
		}
		names = append(names, block["name"].(string))
		msgs += fmt.Sprintf(`,{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}`,
			block["id"], block["name"], block["id"])
	}
	if got := strings.Join(names, ","); got != "a,b,a,text" {
		t.Errorf("turn sequence = %s, want a,b,a,text", got)
	}
}
