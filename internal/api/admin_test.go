package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/store"
	"github.com/mp2209/pitwall/internal/store/storetest"
	"github.com/mp2209/pitwall/mockprovider"
)

const adminToken = "pw-admin-test-token"

type fullStack struct {
	url   string
	rec   *captureRecorder
	redis *miniredis.Miniredis
}

// newFullStack runs the gateway as production does: Postgres store and
// request log, virtual keys, prices synced from the price file, budgets.
func newFullStack(t *testing.T) *fullStack { return newFullStackWith(t, "") }

// newFullStackWith adds extra YAML (for example a guard section) to the
// gateway config.
func newFullStackWith(t *testing.T, extra string) *fullStack {
	t.Helper()
	st := storetest.New(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mock := httptest.NewServer(mockprovider.New(mockprovider.Config{}).Handler())
	t.Cleanup(mock.Close)
	cfg, err := config.Parse(fmt.Appendf(nil, `
providers:
  - {name: mock-openai, kind: openai, base_url: %[1]s}
  - {name: mock-anthropic, kind: anthropic, base_url: %[1]s}
models:
  - {match: mock-gpt, provider: mock-openai}
  - {match: mock-claude, provider: mock-anthropic}
  - {match: new-claude, provider: mock-anthropic}
%[2]s`, mock.URL, extra))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	filePrices, err := config.LoadPrices("../../config/prices.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncPrices(ctx, filePrices); err != nil {
		t.Fatal(err)
	}
	dbPrices, err := st.ListPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer := store.NewRequestWriter(st, log, store.WriterOptions{FlushEvery: 10 * time.Millisecond})
	t.Cleanup(func() { _ = writer.Close(context.Background()) })
	mr := miniredis.RunT(t)
	f := &fullStack{rec: newCapture(), redis: mr}
	srv := New(cfg, Options{
		Log: log, Store: st, AdminToken: adminToken,
		Keys:     auth.NewAuthenticator(st, time.Minute),
		Prices:   metering.NewPriceBook(dbPrices),
		Budget:   metering.NewBudget(redis.NewClient(&redis.Options{Addr: mr.Addr()}), st.SpendSince),
		Recorder: Recorders{f.rec, DBRecorder{W: writer}},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

func (f *fullStack) admin(t *testing.T, method, path, body, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, f.url+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func (f *fullStack) mustAdmin(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	status, v := f.admin(t, method, path, body, adminToken)
	if status != want {
		t.Fatalf("%s %s: status %d, want %d: %v", method, path, status, want, v)
	}
	return v
}

func TestAdminFlowAndCostRows(t *testing.T) {
	f := newFullStack(t)

	if status, _ := f.admin(t, "GET", "/admin/keys", "", ""); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}
	if status, _ := f.admin(t, "GET", "/admin/keys", "", "pw-wrong"); status != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", status)
	}

	team := f.mustAdmin(t, "POST", "/admin/teams", `{"name":"core","monthly_budget_usd":50}`, 201)
	teamID := int64(team["id"].(float64))
	dev := f.mustAdmin(t, "POST", "/admin/users", fmt.Sprintf(`{"email":"dev@example.test","team_id":%d}`, teamID), 201)
	boss := f.mustAdmin(t, "POST", "/admin/users", `{"email":"lead@example.test","role":"admin"}`, 201)
	f.mustAdmin(t, "POST", "/admin/users", `{"email":"dev@example.test"}`, http.StatusConflict)
	f.mustAdmin(t, "POST", "/admin/users", `{"email":"x@example.test","role":"root"}`, http.StatusBadRequest)
	f.mustAdmin(t, "POST", "/admin/keys", `{"user_id":1,"allowed_models":["["]}`, http.StatusBadRequest)

	devKey := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v,"name":"laptop","monthly_budget_usd":5}`, dev["id"]), 201)
	plain := devKey["key"].(string)
	keyID := int64(devKey["id"].(float64))
	if !strings.HasPrefix(plain, "pw-") || devKey["prefix"] != plain[:11] {
		t.Fatalf("created key = %v", devKey)
	}
	bossKey := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, boss["id"]), 201)["key"].(string)

	// A dev key cannot use the admin API; an admin user's key can.
	if status, _ := f.admin(t, "GET", "/admin/keys", "", plain); status != http.StatusForbidden {
		t.Errorf("dev key on admin API: %d", status)
	}
	if status, _ := f.admin(t, "GET", "/admin/keys", "", bossKey); status != http.StatusOK {
		t.Errorf("admin user's key on admin API: %d", status)
	}

	// Two calls, one per client format, each recorded with its cost.
	if resp := post(t, f.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 200), "x-api-key", plain); resp.StatusCode != 200 {
		t.Fatalf("anthropic call: %d", resp.StatusCode)
	}
	if resp := post(t, f.url+"/v1/chat/completions", `{"model":"mock-gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Authorization", "Bearer "+plain); resp.StatusCode != 200 {
		t.Fatalf("openai call: %d", resp.StatusCode)
	} else {
		_, _ = io.ReadAll(resp.Body)
	}
	recs := []Record{f.rec.next(t), f.rec.next(t)}
	var total metering.Nanos
	byID := map[string]Record{}
	for _, r := range recs {
		total += r.Cost
		byID[r.ID] = r
	}

	var rows []any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v := f.mustAdmin(t, "GET", fmt.Sprintf("/admin/requests?key_id=%d", keyID), "", 200)
		if rows, _ = v["data"].([]any); len(rows) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 request rows, got %d", len(rows))
	}
	for _, raw := range rows {
		row := raw.(map[string]any)
		r, ok := byID[row["id"].(string)]
		if !ok {
			t.Fatalf("row %v has no matching call", row["id"])
		}
		if metering.FromUSD(row["cost_usd"].(float64)) != r.Cost || !row["priced"].(bool) || r.Cost <= 0 ||
			int64(row["input_tokens"].(float64)) != r.Usage.InputTokens ||
			int64(row["output_tokens"].(float64)) != r.Usage.OutputTokens ||
			int64(row["team_id"].(float64)) != teamID {
			t.Errorf("row %v does not match call %+v", row, r)
		}
	}

	keys := f.mustAdmin(t, "GET", "/admin/keys", "", 200)["data"].([]any)
	devRow := keys[0].(map[string]any)
	if metering.FromUSD(devRow["spent_this_month_usd"].(float64)) != total || devRow["reserved_usd"].(float64) != 0 {
		t.Errorf("key spend %v, want %v", devRow["spent_this_month_usd"], total.USD())
	}

	// If Redis loses its counters, they are rebuilt from the request log.
	f.redis.FlushAll()
	keys = f.mustAdmin(t, "GET", "/admin/keys", "", 200)["data"].([]any)
	if got := metering.FromUSD(keys[0].(map[string]any)["spent_this_month_usd"].(float64)); got != total {
		t.Errorf("after Redis flush, spend %v, want %v reseeded from Postgres", got.USD(), total.USD())
	}
	teams := f.mustAdmin(t, "GET", "/admin/teams", "", 200)["data"].([]any)
	if got := metering.FromUSD(teams[0].(map[string]any)["spent_this_month_usd"].(float64)); got != total {
		t.Errorf("team spend %v, want %v", got.USD(), total.USD())
	}

	// Key changes apply at once.
	f.mustAdmin(t, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID), `{"allowed_models":["mock-gpt"]}`, 200)
	if resp := post(t, f.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 10), "x-api-key", plain); resp.StatusCode != http.StatusForbidden {
		t.Errorf("after allow-list change: %d", resp.StatusCode)
	}
	f.mustAdmin(t, "PATCH", fmt.Sprintf("/admin/keys/%d", keyID), `{"allowed_models":[],"monthly_budget_usd":0.000001}`, 200)
	if resp := post(t, f.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 10), "x-api-key", plain); resp.StatusCode != http.StatusPaymentRequired {
		t.Errorf("after budget cut: %d", resp.StatusCode)
	}
	f.mustAdmin(t, "DELETE", fmt.Sprintf("/admin/keys/%d", keyID), "", 200)
	if resp := post(t, f.url+"/v1/messages", fmt.Sprintf(anthropicBody, "mock-claude", 10), "x-api-key", plain); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("after revoke: %d", resp.StatusCode)
	}
	f.mustAdmin(t, "DELETE", "/admin/keys/9999", "", http.StatusNotFound)
}

func TestAddedPriceAppliesToLaterCalls(t *testing.T) {
	f := newFullStack(t)
	user := f.mustAdmin(t, "POST", "/admin/users", `{"email":"dev@example.test"}`, 201)
	key := f.mustAdmin(t, "POST", "/admin/keys", fmt.Sprintf(`{"user_id":%v}`, user["id"]), 201)["key"].(string)

	call := func() Record {
		if resp := post(t, f.url+"/v1/messages", fmt.Sprintf(anthropicBody, "new-claude", 100), "x-api-key", key); resp.StatusCode != 200 {
			t.Fatalf("call: %d", resp.StatusCode)
		}
		return f.rec.next(t)
	}
	if r := call(); r.Priced {
		t.Fatalf("new-claude has no price yet: %+v", r)
	}
	f.mustAdmin(t, "POST", "/admin/prices", `{"model":"new-claude","input":1,"output":5}`, 201)
	f.mustAdmin(t, "POST", "/admin/prices", `{"model":"x","speed":"turbo","input":1}`, http.StatusBadRequest)
	r := call()
	want := metering.Price{Rates: metering.Rates{Input: 1, Output: 5}}.Cost(r.Usage)
	if !r.Priced || r.Cost != want {
		t.Errorf("after adding a price: priced %v cost %v, want %v", r.Priced, r.Cost, want)
	}
	prices := f.mustAdmin(t, "GET", "/admin/prices", "", 200)["data"].([]any)
	if len(prices) < 30 {
		t.Errorf("price listing has %d rows", len(prices))
	}
}
