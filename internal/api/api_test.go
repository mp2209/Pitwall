package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/mockprovider"
)

// captureRecorder keeps every record for assertions.
type captureRecorder struct {
	mu   sync.Mutex
	recs []Record
	ch   chan Record
}

func newCapture() *captureRecorder { return &captureRecorder{ch: make(chan Record, 100)} }

func (c *captureRecorder) Record(_ context.Context, r Record) {
	c.mu.Lock()
	c.recs = append(c.recs, r)
	c.mu.Unlock()
	c.ch <- r
}

func (c *captureRecorder) next(t *testing.T) Record {
	t.Helper()
	select {
	case r := <-c.ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no record written")
		return Record{}
	}
}

// newGateway starts a gateway whose mock-openai and mock-anthropic
// providers both point at upstreamURL.
func newGateway(t *testing.T, upstreamURL string, devKeys ...string) (*httptest.Server, *captureRecorder) {
	t.Helper()
	cfg, err := config.Parse(fmt.Appendf(nil, `
dev_keys: [%s]
providers:
  - {name: mock-openai, kind: openai, base_url: %[2]s}
  - {name: mock-anthropic, kind: anthropic, base_url: %[2]s}
models:
  - {match: gpt-mock, provider: mock-openai}
  - {match: "claude-*", provider: mock-anthropic, upstream: claude-upstream}
`, strings.Join(devKeys, ","), upstreamURL))
	if err != nil {
		t.Fatal(err)
	}
	rec := newCapture()
	srv := New(cfg, Options{Recorder: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, rec
}

func newMockGateway(t *testing.T, devKeys ...string) (*httptest.Server, *captureRecorder) {
	t.Helper()
	mock := httptest.NewServer(mockprovider.New(mockprovider.Config{}).Handler())
	t.Cleanup(mock.Close)
	return newGateway(t, mock.URL, devKeys...)
}

func post(t *testing.T, url, body string, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

const toolsOpenAI = `"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`
const toolsAnthropic = `"tools":[{"name":"bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`

func TestAllFormatPairsRecordUsage(t *testing.T) {
	ts, rec := newMockGateway(t)
	cases := []struct {
		name, path, body, provider, upstreamModel string
	}{
		{"openai->openai", "/v1/chat/completions", `{"model":"gpt-mock","messages":[{"role":"user","content":"hi"}],` + toolsOpenAI + `}`, "mock-openai", "gpt-mock"},
		{"openai->anthropic", "/v1/chat/completions", `{"model":"claude-a","messages":[{"role":"user","content":"hi"}],` + toolsOpenAI + `}`, "mock-anthropic", "claude-upstream"},
		{"anthropic->anthropic", "/v1/messages", `{"model":"claude-a","max_tokens":50,"messages":[{"role":"user","content":"hi"}],` + toolsAnthropic + `}`, "mock-anthropic", "claude-upstream"},
		{"anthropic->openai", "/v1/messages", `{"model":"gpt-mock","max_tokens":50,"messages":[{"role":"user","content":"hi"}],` + toolsAnthropic + `}`, "mock-openai", "gpt-mock"},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				body := tc.body
				if stream {
					body = strings.Replace(body, `{`, `{"stream":true,`, 1)
				}
				resp := post(t, ts.URL+tc.path, body)
				if resp.StatusCode != http.StatusOK {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("status %d: %s", resp.StatusCode, b)
				}
				if resp.Header.Get("x-pitwall-request-id") == "" {
					t.Error("missing request id header")
				}
				raw, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(raw), "bash") || !strings.Contains(string(raw), "cmd") {
					t.Errorf("tool call missing from response: %s", raw)
				}
				r := rec.next(t)
				if r.Provider != tc.provider || r.UpstreamModel != tc.upstreamModel || r.Stream != stream || r.Status != 200 {
					t.Errorf("record = %+v", r)
				}
				if r.Usage.Estimated || r.Usage.PromptTokens() == 0 || r.Usage.OutputTokens == 0 {
					t.Errorf("usage should come from the provider: %+v", r.Usage)
				}
			})
		}
	}
}

func TestAuth(t *testing.T) {
	ts, _ := newMockGateway(t, "pw-good")
	body := `{"model":"gpt-mock","messages":[{"role":"user","content":"hi"}]}`

	resp := post(t, ts.URL+"/v1/chat/completions", body, "Authorization", "Bearer pw-bad")
	if resp.StatusCode != 401 || readJSON(t, resp)["error"].(map[string]any)["type"] != "authentication_error" {
		t.Errorf("bad key: status %d", resp.StatusCode)
	}
	resp = post(t, ts.URL+"/v1/messages", `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if v := readJSON(t, resp); resp.StatusCode != 401 || v["type"] != "error" {
		t.Errorf("anthropic clients should get anthropic-shaped errors: %d %v", resp.StatusCode, v)
	}
	if resp := post(t, ts.URL+"/v1/chat/completions", body, "Authorization", "Bearer pw-good"); resp.StatusCode != 200 {
		t.Errorf("bearer key rejected: %d", resp.StatusCode)
	}
	if resp := post(t, ts.URL+"/v1/messages", `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`, "x-api-key", "pw-good"); resp.StatusCode != 200 {
		t.Errorf("x-api-key rejected: %d", resp.StatusCode)
	}
}

func TestUnknownModelAndBadJSON(t *testing.T) {
	ts, _ := newMockGateway(t)
	resp := post(t, ts.URL+"/v1/chat/completions", `{"model":"nope","messages":[]}`)
	if resp.StatusCode != 404 {
		t.Errorf("unknown model: %d", resp.StatusCode)
	}
	resp = post(t, ts.URL+"/v1/messages", `{not json`)
	if v := readJSON(t, resp); resp.StatusCode != 400 || v["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Errorf("bad json: %d %v", resp.StatusCode, v)
	}
}

func TestUpstreamErrors(t *testing.T) {
	ts, rec := newMockGateway(t)

	// Same format: the provider's error passes through untouched.
	resp := post(t, ts.URL+"/v1/messages", `{"model":"claude-a","max_tokens":5,"messages":[{"role":"user","content":"[[mock:error=529]]"}]}`)
	if v := readJSON(t, resp); resp.StatusCode != 529 || v["error"].(map[string]any)["type"] != "overloaded_error" {
		t.Errorf("passthrough error: %d %v", resp.StatusCode, v)
	}
	if r := rec.next(t); r.Status != 529 || r.Error == "" {
		t.Errorf("record = %+v", r)
	}

	// Cross format: an Anthropic error reaches an OpenAI client in OpenAI shape.
	resp = post(t, ts.URL+"/v1/chat/completions", `{"model":"claude-a","messages":[{"role":"user","content":"[[mock:error=429]]"}]}`)
	v := readJSON(t, resp)
	e, _ := v["error"].(map[string]any)
	if resp.StatusCode != 429 || e["type"] != "rate_limit_error" || e["message"] != "mock provider injected error" {
		t.Errorf("translated error: %d %v", resp.StatusCode, v)
	}
	rec.next(t)
}

func TestProviderUnreachable(t *testing.T) {
	ts, rec := newGateway(t, "http://127.0.0.1:1")
	resp := post(t, ts.URL+"/v1/chat/completions", `{"model":"gpt-mock","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d", resp.StatusCode)
	}
	if r := rec.next(t); r.Status != http.StatusBadGateway {
		t.Errorf("record = %+v", r)
	}
}

// When the agent disconnects mid-stream, the gateway must cancel the
// provider call and still record the tokens used so far.
func TestClientDisconnectCancelsUpstream(t *testing.T) {
	upstreamCancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":77,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_ = rc.Flush()
		for {
			select {
			case <-r.Context().Done():
				close(upstreamCancelled)
				return
			case <-time.After(10 * time.Millisecond):
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"word word \"}}\n\n")
				if rc.Flush() != nil {
					close(upstreamCancelled)
					return
				}
			}
		}
	}))
	defer upstream.Close()
	ts, rec := newGateway(t, upstream.URL)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-a","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"go"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for i := 0; i < 12; i++ { // read a few events, then hang up
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	resp.Body.Close()

	select {
	case <-upstreamCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("provider call was not cancelled")
	}
	r := rec.next(t)
	if !r.ClientGone {
		t.Errorf("record should mark the client gone: %+v", r)
	}
	if r.Usage.InputTokens != 77 || r.Usage.OutputTokens == 0 || !r.Usage.Estimated {
		t.Errorf("usage so far = %+v: want exact input and estimated output", r.Usage)
	}
}

func TestModelsList(t *testing.T) {
	ts, _ := newMockGateway(t)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	v := readJSON(t, resp)
	data, _ := v["data"].([]any)
	if v["object"] != "list" || len(data) != 1 || data[0].(map[string]any)["id"] != "gpt-mock" {
		t.Errorf("models = %v (wildcard rules must not be listed)", v)
	}
}

func TestCountTokens(t *testing.T) {
	ts, _ := newMockGateway(t)
	for _, model := range []string{"claude-a", "gpt-mock"} {
		resp := post(t, ts.URL+"/v1/messages/count_tokens", `{"model":"`+model+`","messages":[{"role":"user","content":"hello"}]}`)
		if v := readJSON(t, resp); resp.StatusCode != 200 || v["input_tokens"].(float64) <= 0 {
			t.Errorf("%s: %d %v", model, resp.StatusCode, v)
		}
	}
}
