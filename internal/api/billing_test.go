package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/mockprovider"
)

type memKeys map[string]*auth.Key

func (m memKeys) KeyByHash(_ context.Context, hash []byte) (*auth.Key, error) {
	if k, ok := m[string(hash)]; ok {
		return k, nil
	}
	return nil, auth.ErrNotFound
}

// billed is a gateway with virtual keys, prices and Redis budgets, in front
// of the mock provider.
type billed struct {
	url    string
	rec    *captureRecorder
	prices *metering.PriceBook
	budget *metering.Budget
	redis  *miniredis.Miniredis
	keys   memKeys
}

func (b *billed) addKey(t *testing.T, k *auth.Key) string {
	t.Helper()
	plain, prefix, hash, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	k.Prefix = prefix
	b.keys[string(hash)] = k
	return plain
}

func newBilled(t *testing.T, failOpen bool) *billed {
	t.Helper()
	mock := httptest.NewServer(mockprovider.New(mockprovider.Config{}).Handler())
	t.Cleanup(mock.Close)
	cfg, err := config.Parse(fmt.Appendf(nil, `
budget_fail_open: %v
providers:
  - {name: mock-openai, kind: openai, base_url: %[2]s}
  - {name: mock-anthropic, kind: anthropic, base_url: %[2]s}
  - {name: local, kind: openai, base_url: %[2]s, free: true}
models:
  - {match: mock-gpt, provider: mock-openai}
  - {match: mock-claude, provider: mock-anthropic}
  - {match: unpriced-claude, provider: mock-anthropic}
  - {match: local-model, provider: local}
`, failOpen, mock.URL))
	if err != nil {
		t.Fatal(err)
	}
	prices, err := config.LoadPrices("../../config/prices.yaml")
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	b := &billed{
		rec:    newCapture(),
		prices: metering.NewPriceBook(prices),
		budget: metering.NewBudget(redis.NewClient(&redis.Options{Addr: mr.Addr()}), nil),
		redis:  mr,
		keys:   memKeys{},
	}
	srv := New(cfg, Options{
		Recorder: b.rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Keys: auth.NewAuthenticator(b.keys, time.Minute), Prices: b.prices, Budget: b.budget,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	b.url = ts.URL
	return b
}

const anthropicBody = `{"model":"%s","max_tokens":%d,"messages":[{"role":"user","content":"hello"}]}`

func TestVirtualKeyRequired(t *testing.T) {
	b := newBilled(t, true)
	body := fmt.Sprintf(anthropicBody, "mock-claude", 100)
	for _, key := range []string{"", "pw-not-a-real-key", "sk-ant-something"} {
		resp := post(t, b.url+"/v1/messages", body, "x-api-key", key)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("key %q: status %d", key, resp.StatusCode)
		}
	}
	revoked := b.addKey(t, &auth.Key{ID: 9, Revoked: true})
	resp := post(t, b.url+"/v1/messages", body, "x-api-key", revoked)
	if v := readJSON(t, resp); resp.StatusCode != 401 || !strings.Contains(v["error"].(map[string]any)["message"].(string), "revoked") {
		t.Errorf("revoked key: %d %v", resp.StatusCode, v)
	}
}

func TestModelAllowList(t *testing.T) {
	b := newBilled(t, true)
	key := b.addKey(t, &auth.Key{ID: 1, AllowedModels: []string{"mock-gpt"}})
	resp := post(t, b.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 100), "x-api-key", key)
	if v := readJSON(t, resp); resp.StatusCode != http.StatusForbidden || v["error"].(map[string]any)["type"] != "permission_error" {
		t.Errorf("disallowed model: %d %v", resp.StatusCode, v)
	}
	if resp := post(t, b.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-gpt", 100), "x-api-key", key); resp.StatusCode != 200 {
		t.Errorf("allowed model: %d", resp.StatusCode)
	}
}

func TestEveryCallGetsACostAndSettles(t *testing.T) {
	b := newBilled(t, true)
	key := b.addKey(t, &auth.Key{ID: 3, TeamID: 5, TeamName: "core", MonthlyBudget: metering.FromUSD(10)})
	p, _ := b.prices.Lookup("mock-claude", "", time.Now())

	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"mock-claude","max_tokens":500,"stream":%v,"system":[{"type":"text","text":%q,"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`,
			stream, strings.Repeat("You are a careful agent. ", 300))
		resp := post(t, b.url+"/v1/messages", body, "x-api-key", key)
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %s", resp.StatusCode, raw)
		}
		r := b.rec.next(t)
		if !r.Priced || r.Cost <= 0 || r.Cost != p.Cost(r.Usage) || r.Usage.Estimated {
			t.Errorf("stream=%v: record cost %v priced %v, want %v from usage %+v", stream, r.Cost, r.Priced, p.Cost(r.Usage), r.Usage)
		}
		if r.KeyID != 3 || r.TeamID != 5 || r.Reserved < r.Cost {
			t.Errorf("stream=%v: key/team/reserved wrong: %+v", stream, r)
		}
		if !stream {
			got, _ := strconv.ParseFloat(resp.Header.Get("x-pitwall-cost-usd"), 64)
			if metering.FromUSD(got) != r.Cost {
				t.Errorf("cost header %q, record %v", resp.Header.Get("x-pitwall-cost-usd"), r.Cost)
			}
		}
	}
	// Both calls settled: nothing held, spend equals the two recorded costs,
	// on the key and on its team.
	var total metering.Nanos
	for _, r := range b.rec.recs {
		total += r.Cost
	}
	for _, scope := range []string{"key:3", "team:5"} {
		spent, held, err := b.budget.Status(context.Background(), scope)
		if err != nil || spent != total || held != 0 {
			t.Errorf("%s: spent %v held %v (want %v, 0): %v", scope, spent, held, total, err)
		}
	}
}

func TestBudgetExceededIsA402InTheClientsFormat(t *testing.T) {
	b := newBilled(t, true)
	// One mock-claude call with max_tokens 1000 can cost up to about $0.015
	// in output alone, so a 1-cent budget refuses it before it is sent.
	key := b.addKey(t, &auth.Key{ID: 4, MonthlyBudget: metering.FromUSD(0.01)})

	resp := post(t, b.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 1000), "x-api-key", key)
	v := readJSON(t, resp)
	e, _ := v["error"].(map[string]any)
	if resp.StatusCode != http.StatusPaymentRequired || v["type"] != "error" || e["type"] != "billing_error" ||
		!strings.Contains(e["message"].(string), "pw-") {
		t.Errorf("anthropic client: %d %v", resp.StatusCode, v)
	}
	resp = post(t, b.url+"/v1/chat/completions", `{"model":"mock-claude","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer "+key)
	v = readJSON(t, resp)
	if e, _ := v["error"].(map[string]any); resp.StatusCode != 402 || e["type"] != "insufficient_quota" {
		t.Errorf("openai client: %d %v", resp.StatusCode, v)
	}
	r := b.rec.next(t)
	if r.Status != 402 || r.Cost != 0 || r.KeyID != 4 {
		t.Errorf("refused call should be recorded at $0: %+v", r)
	}
	if _, held, _ := b.budget.Status(context.Background(), "key:4"); held != 0 {
		t.Errorf("refused calls must not hold money: %v", held)
	}
	// A small max_tokens fits the budget.
	if resp := post(t, b.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 50), "x-api-key", key); resp.StatusCode != 200 {
		t.Errorf("small call should fit: %d", resp.StatusCode)
	}
}

func TestFreeAndUnpricedModels(t *testing.T) {
	b := newBilled(t, true)
	key := b.addKey(t, &auth.Key{ID: 6, MonthlyBudget: metering.FromUSD(0.000001)})
	resp := post(t, b.url+"/v1/chat/completions", `{"model":"local-model","messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer "+key)
	if resp.StatusCode != 200 || resp.Header.Get("x-pitwall-cost-usd") != "0.000000000" {
		t.Errorf("free provider: %d cost %q", resp.StatusCode, resp.Header.Get("x-pitwall-cost-usd"))
	}
	if r := b.rec.next(t); !r.Priced || r.Cost != 0 {
		t.Errorf("free provider record: %+v", r)
	}
	resp = post(t, b.url+"/v1/messages", fmt.Sprintf(anthropicBody, "unpriced-claude", 50), "x-api-key", key)
	if resp.StatusCode != 200 || resp.Header.Get("x-pitwall-cost-usd") != "" {
		t.Errorf("unpriced model: %d cost header %q", resp.StatusCode, resp.Header.Get("x-pitwall-cost-usd"))
	}
	if r := b.rec.next(t); r.Priced || r.Cost != 0 {
		t.Errorf("unpriced record should be flagged: %+v", r)
	}
}

func TestRedisDownFailOpenAndClosed(t *testing.T) {
	for _, failOpen := range []bool{true, false} {
		b := newBilled(t, failOpen)
		key := b.addKey(t, &auth.Key{ID: 8, MonthlyBudget: metering.FromUSD(5)})
		b.redis.Close()
		resp := post(t, b.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 50), "x-api-key", key)
		want := http.StatusOK
		if !failOpen {
			want = http.StatusServiceUnavailable
		}
		if resp.StatusCode != want {
			t.Errorf("fail_open=%v: status %d, want %d", failOpen, resp.StatusCode, want)
		}
		if r := b.rec.next(t); failOpen && (r.Cost == 0 || !r.Priced) {
			t.Errorf("fail-open call should still be priced: %+v", r)
		}
	}
}

func TestModelsListFollowsAllowList(t *testing.T) {
	b := newBilled(t, true)
	key := b.addKey(t, &auth.Key{ID: 2, AllowedModels: []string{"mock-*"}})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, b.url+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if len(v.Data) != 2 {
		t.Errorf("models = %+v, want only mock-gpt and mock-claude", v.Data)
	}
}

type panickyRecorder struct{}

func (panickyRecorder) Record(context.Context, Record) { panic("boom") }

func TestRecordersIsolatePanics(t *testing.T) {
	first, last := newCapture(), newCapture()
	Recorders{first, panickyRecorder{}, last}.Record(context.Background(), Record{ID: "pw_1"})
	if first.next(t).ID != "pw_1" || last.next(t).ID != "pw_1" {
		t.Fatal("recorders around a panicking one must still run")
	}
}
