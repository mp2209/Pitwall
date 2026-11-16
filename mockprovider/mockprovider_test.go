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
