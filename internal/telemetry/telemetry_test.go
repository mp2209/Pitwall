package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mp2209/pitwall/internal/api"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/tasks"
)

// Credential-shaped test strings are assembled at runtime, so the source
// never holds anything a secret scanner would mistake for a real key.
func fakeKey(prefix string, n int) string {
	return prefix + strings.Repeat("Ab3", n/3+1)[:n]
}

func TestRedact(t *testing.T) {
	anthropicKey := fakeKey("sk-"+"ant-api03-", 40)
	openAIKey := fakeKey("sk-"+"proj-", 30)
	pitwallKey := fakeKey("pw"+"-", 32)
	awsID := "AK" + "IA" + strings.Repeat("Q", 16)
	githubToken := fakeKey("gh"+"p_", 36)
	bearer := "Bearer " + fakeKey("eyJ", 40)
	privateKey := "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEow\n-----END RSA " + "PRIVATE KEY-----"
	redacted := map[string]string{
		"mail me at jane.doe+work@example.co.uk please": "mail me at [REDACTED:email] please",
		"card 4111 1111 1111 1111 exp 12/29":            "card [REDACTED:card] exp 12/29",
		"card 4111-1111-1111-1111":                      "card [REDACTED:card]",
		"call (555) 123-4567 or +1 555 123 4567":        "call [REDACTED:phone] or [REDACTED:phone]",
		"export ANTHROPIC_API_KEY=" + anthropicKey:      "export ANTHROPIC_API_KEY=[REDACTED:secret]",
		"OPENAI_API_KEY=" + openAIKey:                   "OPENAI_API_KEY=[REDACTED:secret]",
		"key " + pitwallKey + " leaked":                 "key [REDACTED:secret] leaked",
		"aws " + awsID + " here":                        "aws [REDACTED:secret] here",
		"Authorization: " + bearer:                      "Authorization: [REDACTED:secret]",
		"token " + githubToken:                          "token [REDACTED:secret]",
		privateKey:                                      "[REDACTED:secret]",
	}
	for in, want := range redacted {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	// Things that look numeric or key-like but are not personal data.
	kept := []string{
		"2026-11-18T10:00:00Z took 1234567890123 ns",
		"order 1234 5678 9012 3456 is not a valid card",
		"version 1.25.0, port 5432, line 1024",
		"sha 4f2a9c1e8b7d6a5f4e3d2c1b0a9f8e7d",
		"scikit-learn and sk-short",
		"func f(a, b int) { return 555 }",
	}
	for _, in := range kept {
		if got := Redact(in); got != in {
			t.Errorf("Redact(%q) changed it to %q", in, got)
		}
	}
	// Redacting inside JSON keeps it valid.
	js := `{"text":"reach me at bob@example.com, key ` + anthropicKey + `"}`
	if out := Redact(js); !json.Valid([]byte(out)) || strings.Contains(out, "bob@") || strings.Contains(out, anthropicKey) {
		t.Errorf("JSON redaction: %s", out)
	}
}

type fakeLangfuse struct {
	mu     sync.Mutex
	events []map[string]any
	auth   string
}

func (f *fakeLangfuse) handler(w http.ResponseWriter, r *http.Request) {
	user, pass, _ := r.BasicAuth()
	var body struct {
		Batch []map[string]any `json:"batch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.auth = user + ":" + pass
	f.events = append(f.events, body.Batch...)
	f.mu.Unlock()
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, `{"successes":[],"errors":[]}`)
}

func TestLangfuseTracePerTaskGenerationPerCall(t *testing.T) {
	fake := &fakeLangfuse{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()
	lf := NewLangfuse(LangfuseConfig{Host: srv.URL + "/", PublicKey: "pk-lf-test", SecretKey: "sk-lf-test", FlushEvery: 10 * time.Millisecond},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	start := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	base := api.Record{
		TaskID: "t_abc", TaskSource: "header", TaskExternalID: "fix-parser-1", Agent: "pitwall-agent",
		ConfigLabel: "sonnet/full", KeyPrefix: "pw-abcd1234", Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5",
		ClientFormat: "anthropic", Status: 200, Stream: true, Priced: true, Cost: metering.FromUSD(0.0123),
		Usage: metering.Usage{InputTokens: 10, CachedReadTokens: 900, CacheWriteTokens: 50, OutputTokens: 40},
	}
	for i := 1; i <= 3; i++ {
		r := base
		r.ID, r.CallIndex, r.TaskStart = "pw_"+string(rune('0'+i)), i, start.Add(time.Minute)
		r.Time, r.Latency, r.TTFT = start.Add(time.Duration(i)*time.Minute), 2*time.Second, 300*time.Millisecond
		r.NewMessages = []tasks.Message{{Role: "user", Content: json.RawMessage(`"my email is dev@example.com"`)}}
		r.OutputText = "Running the tests"
		lf.Record(context.Background(), r)
	}
	failed := base
	failed.ID, failed.CallIndex, failed.Status, failed.Error = "pw_4", 4, 529, "overloaded, contact ops@example.com"
	failed.TaskStart = start.Add(time.Minute)
	failed.Time = start.Add(5 * time.Minute)
	lf.Record(context.Background(), failed)
	if err := lf.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.auth != "pk-lf-test:sk-lf-test" {
		t.Errorf("auth = %q", fake.auth)
	}
	var traces, gens []map[string]any
	for _, e := range fake.events {
		body := e["body"].(map[string]any)
		switch e["type"] {
		case "trace-create":
			traces = append(traces, body)
		case "generation-create":
			gens = append(gens, body)
		}
	}
	// Every call upserts the same trace, with the task's start time, so the
	// trace exists whichever call Langfuse sees first.
	if len(traces) != 4 {
		t.Fatalf("want a trace upsert per call, got %d", len(traces))
	}
	for _, tr := range traces {
		if tr["id"] != "t_abc" || tr["name"] != "fix-parser-1" || tr["userId"] != "pw-abcd1234" || tr["timestamp"] != "2026-11-18T10:01:00.000Z" {
			t.Fatalf("trace upsert = %v", tr)
		}
	}
	if len(gens) != 4 {
		t.Fatalf("want 4 generations, got %d", len(gens))
	}
	g := gens[0]
	if g["traceId"] != "t_abc" || g["model"] != "claude-sonnet-5-5" || g["startTime"] != "2026-11-18T10:01:00.000Z" ||
		g["endTime"] != "2026-11-18T10:01:02.000Z" || g["completionStartTime"] != "2026-11-18T10:01:00.300Z" {
		t.Errorf("generation = %v", g)
	}
	in, _ := json.Marshal(g["input"])
	if strings.Contains(string(in), "dev@example.com") || !strings.Contains(string(in), "[REDACTED:email]") {
		t.Errorf("input not redacted: %s", in)
	}
	usage := g["usageDetails"].(map[string]any)
	if usage["input"].(float64) != 10 || usage["cache_read_input_tokens"].(float64) != 900 || usage["output"].(float64) != 40 {
		t.Errorf("usage = %v", usage)
	}
	if g["costDetails"].(map[string]any)["total"].(float64) != 0.0123 {
		t.Errorf("cost = %v", g["costDetails"])
	}
	last := gens[3]
	if last["level"] != "ERROR" || strings.Contains(last["statusMessage"].(string), "ops@example.com") {
		t.Errorf("failed call = %v", last)
	}
	if sent, dropped, failedN := lf.Stats(); sent != 8 || dropped != 0 || failedN != 0 {
		t.Errorf("stats sent %d dropped %d failed %d", sent, dropped, failedN)
	}
}

func TestLangfuseCapsLargeFields(t *testing.T) {
	lf := &Langfuse{cfg: LangfuseConfig{MaxField: 100}}
	got := lf.field(`"` + strings.Repeat("x", 500) + `"`)
	s, ok := got.(string)
	if !ok || len(s) > 160 || !strings.Contains(s, "more bytes not sent") {
		t.Errorf("large field = %v", got)
	}
	if _, ok := lf.field(`{"a":1}`).(json.RawMessage); !ok {
		t.Error("small JSON should stay JSON")
	}
}

func TestMetricsLabelsStayBounded(t *testing.T) {
	m := NewMetrics()
	for i, model := range []string{"claude-sonnet-5-5", "made-up-model-1", "made-up-model-2"} {
		m.Record(context.Background(), api.Record{
			TaskID: "t_secret_task_id", TaskSource: "inferred", CallIndex: 1,
			Provider: "anthropic", UpstreamModel: model, ClientFormat: "anthropic", Status: 200,
			Priced: i == 0, Stream: true, TTFT: time.Second, Latency: 2 * time.Second, Overhead: time.Millisecond,
			Usage: metering.Usage{InputTokens: 100, CachedReadTokens: 1000, OutputTokens: 10},
			Cost:  metering.FromUSD(0.01), CacheSavings: metering.FromUSD(0.002), RepeatedInputTokens: 900,
		})
	}
	m.Record(context.Background(), api.Record{Provider: "anthropic", UpstreamModel: "claude-sonnet-5-5", ClientFormat: "openai", Status: 402})
	// A call whose cache writes cost more than caching saved: negative
	// savings must not panic a counter.
	m.Record(context.Background(), api.Record{Provider: "anthropic", UpstreamModel: "claude-haiku-5-5", ClientFormat: "anthropic",
		Status: 200, Priced: true, Cost: metering.FromUSD(0.00125), CacheSavings: -metering.FromUSD(0.00025),
		Usage: metering.Usage{CacheWriteTokens: 10000}})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`pitwall_tokens_total{kind="cached_read",model="claude-sonnet-5-5",provider="anthropic"} 1000`,
		`pitwall_cost_usd_total{model="other",provider="anthropic"} 0.02`,
		`pitwall_cost_without_cache_usd_total{model="claude-sonnet-5-5",provider="anthropic"} 0.012`,
		`pitwall_repeated_input_tokens_total{model="claude-sonnet-5-5",provider="anthropic"} 900`,
		`pitwall_tasks_started_total{source="inferred"} 3`,
		`pitwall_budget_refusals_total 1`,
		`pitwall_cost_without_cache_usd_total{model="claude-haiku-5-5",provider="anthropic"} 0.001`,
		`pitwall_calls_total{format="openai",model="other",provider="anthropic",status="402"} 1`,
		`pitwall_gateway_overhead_seconds_count{format="anthropic",stream="true"} 3`,
		`pitwall_ttft_seconds_count{model="claude-sonnet-5-5",provider="anthropic"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	for _, leak := range []string{"t_secret_task_id", "made-up-model"} {
		if strings.Contains(body, leak) {
			t.Errorf("%q must not become a label", leak)
		}
	}
}
