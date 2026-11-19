package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/store"
	"github.com/mp2209/pitwall/internal/store/storetest"
)

var ctx = context.Background()

func TestMigrateIsIdempotentAndConcurrent(t *testing.T) {
	st := storetest.New(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- st.Migrate(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestTeamsUsersKeys(t *testing.T) {
	st := storetest.New(t)
	team, err := st.CreateTeam(ctx, "core", 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTeam(ctx, "core", 1); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate team: %v", err)
	}
	user, err := st.CreateUser(ctx, "dev@example.test", &team.ID, auth.RoleDev)
	if err != nil {
		t.Fatal(err)
	}
	missing := int64(999)
	if _, err := st.CreateUser(ctx, "x@example.test", &missing, auth.RoleDev); !errors.Is(err, store.ErrBadRef) {
		t.Errorf("unknown team: %v", err)
	}

	info, plain, err := st.CreateKey(ctx, store.NewKey{UserID: user.ID, Name: "laptop", AllowedModels: []string{"claude-*"}, MonthlyBudgetUSD: 25})
	if err != nil {
		t.Fatal(err)
	}
	if info.Prefix != plain[:11] || info.UserEmail != "dev@example.test" || *info.TeamID != team.ID {
		t.Errorf("key info = %+v", info)
	}

	k, err := st.KeyByHash(ctx, auth.HashKey(plain))
	if err != nil {
		t.Fatal(err)
	}
	if k.UserID != user.ID || k.TeamName != "core" || k.MonthlyBudget != metering.FromUSD(25) ||
		k.TeamBudget != metering.FromUSD(100) || len(k.AllowedModels) != 1 || k.Revoked {
		t.Errorf("key = %+v", k)
	}
	if _, err := st.KeyByHash(ctx, auth.HashKey("pw-nope")); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("unknown hash: %v", err)
	}

	on := true
	models := []string{}
	upd, err := st.UpdateKey(ctx, info.ID, store.KeyUpdate{CacheOptimizerOn: &on, AllowedModels: &models})
	if err != nil {
		t.Fatal(err)
	}
	if !upd.CacheOptimizerOn || len(upd.AllowedModels) != 0 || upd.Name != "laptop" || upd.MonthlyBudgetUSD != 25 {
		t.Errorf("update changed the wrong fields: %+v", upd)
	}
	if _, err := st.UpdateKey(ctx, 999, store.KeyUpdate{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update missing key: %v", err)
	}

	rev, err := st.RevokeKey(ctx, info.ID)
	if err != nil || rev.RevokedAt == nil {
		t.Fatalf("revoke: %v %+v", err, rev)
	}
	if k, _ := st.KeyByHash(ctx, auth.HashKey(plain)); !k.Revoked {
		t.Error("revoked key should be marked")
	}
}

func TestPricesSyncNeverOverwrites(t *testing.T) {
	st := storetest.New(t)
	haiku := metering.Price{
		Model: "claude-haiku-5-5", Rates: metering.Rates{Input: 0.10, CachedRead: 0.01, Output: 0.50},
		LongContextThreshold: 100000, LongContext: &metering.Rates{Input: 0.50, Output: 2.50}, USMultiplier: 1.1,
	}
	n, err := st.SyncPrices(ctx, []metering.Price{haiku, {Model: "x", Speed: "fast", Rates: metering.Rates{Input: 1, Output: 2}}})
	if err != nil || n != 2 {
		t.Fatalf("sync: %d %v", n, err)
	}
	changed := haiku
	changed.Input = 99
	if n, err := st.SyncPrices(ctx, []metering.Price{changed}); err != nil || n != 0 {
		t.Fatalf("re-sync inserted %d (%v); existing rows must not change", n, err)
	}
	if err := st.AddPrice(ctx, changed); !errors.Is(err, store.ErrConflict) {
		t.Errorf("AddPrice duplicate: %v", err)
	}
	later := changed
	later.EffectiveFrom = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := st.AddPrice(ctx, later); err != nil {
		t.Fatal(err)
	}

	prices, err := st.ListPrices(ctx)
	if err != nil || len(prices) != 3 {
		t.Fatalf("list: %v %d", err, len(prices))
	}
	book := metering.NewPriceBook(prices)
	p, ok := book.Lookup("claude-haiku-5-5", "", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC))
	if !ok || p.Input != 0.10 || p.LongContext == nil || p.LongContext.Output != 2.50 || p.USMultiplier != 1.1 {
		t.Errorf("round-tripped price = %+v", p)
	}
	if p, _ := book.Lookup("claude-haiku-5-5", "", later.EffectiveFrom.Add(time.Hour)); p.Input != 99 {
		t.Errorf("later row should apply after its date: %+v", p)
	}
}

func TestRequestWriterAndSpend(t *testing.T) {
	st := storetest.New(t)
	team, _ := st.CreateTeam(ctx, "core", 0)
	user, _ := st.CreateUser(ctx, "dev@example.test", &team.ID, auth.RoleDev)
	key, _, _ := st.CreateKey(ctx, store.NewKey{UserID: user.ID})

	w := store.NewRequestWriter(st, slog.New(slog.NewTextHandler(io.Discard, nil)), store.WriterOptions{BatchSize: 7, FlushEvery: 20 * time.Millisecond})
	now := time.Now()
	ttft := 42
	for i := range 25 {
		w.Enqueue(store.RequestRow{
			ID: "pw_" + string(rune('a'+i)), Time: now, KeyID: key.ID, TeamID: &team.ID,
			ClientFormat: "anthropic", Model: "m", UpstreamModel: "m", Provider: "p", Status: 200,
			InputTokens: 10, Cost: metering.Nanos(123_456_789), Priced: true, TTFTMS: &ttft,
		})
	}
	old := store.RequestRow{ID: "pw_old", Time: now.AddDate(0, -2, 0), KeyID: key.ID, TeamID: &team.ID,
		ClientFormat: "openai", Model: "m", UpstreamModel: "m", Provider: "p", Status: 200, Cost: metering.FromUSD(5)}
	w.Enqueue(old)
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if written, dropped := w.Stats(); written != 26 || dropped != 0 {
		t.Fatalf("written %d dropped %d", written, dropped)
	}
	if w.Enqueue(old) {
		t.Error("enqueue after close should be refused")
	}

	since := metering.MonthStart(now)
	for _, scope := range []string{auth.KeyScope(key.ID), auth.TeamScope(team.ID)} {
		spent, err := st.SpendSince(ctx, scope, since)
		if err != nil {
			t.Fatal(err)
		}
		if want := metering.Nanos(25 * 123_456_789); spent != want {
			t.Errorf("%s spent %d nanos, want %d (exact, and excluding last-month rows)", scope, spent, want)
		}
	}

	rows, err := st.RecentRequests(ctx, key.ID, 5)
	if err != nil || len(rows) != 5 {
		t.Fatalf("recent: %v %d", err, len(rows))
	}
	if rows[0].Cost != 123_456_789 || rows[0].CostUSD != 0.123456789 || *rows[0].TTFTMS != 42 {
		t.Errorf("row = %+v", rows[0])
	}
}

// An agent may report its outcome before the gateway has written the task's
// last calls; the outcome makes a stub row that the calls then fill in.
func TestTaskOutcomeBeforeCalls(t *testing.T) {
	st := storetest.New(t)
	user, _ := st.CreateUser(ctx, "dev@example.test", nil, auth.RoleDev)
	key, _, _ := st.CreateKey(ctx, store.NewKey{UserID: user.ID})
	other, _ := st.CreateUser(ctx, "other@example.test", nil, auth.RoleDev)
	otherKey, _, _ := st.CreateKey(ctx, store.NewKey{UserID: other.ID})

	ref := store.TaskRef{ID: "t_abc", KeyID: key.ID, Source: "header", ExternalID: "run-1"}
	if err := st.SetTaskOutcome(ctx, ref, store.OutcomeSolved); err != nil {
		t.Fatal(err)
	}
	stolen := ref
	stolen.KeyID = otherKey.ID
	if err := st.SetTaskOutcome(ctx, stolen, store.OutcomeFailed); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another key set the outcome: %v", err)
	}

	start := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	task := "t_abc"
	var rows []store.RequestRow
	for i := range 3 {
		rows = append(rows, store.RequestRow{
			ID: fmt.Sprintf("pw_%d", i), Time: start.Add(time.Duration(i) * 10 * time.Second), TaskID: &task,
			KeyID: key.ID, ClientFormat: "anthropic", Model: "m", UpstreamModel: "m", Provider: "p", Status: 200,
			InputTokens: int64(100 * (i + 1)), CachedReadTokens: int64(1000 * i), OutputTokens: 50,
			RepeatedInputTokens: int64(900 * i), Cost: metering.FromUSD(0.01), LatencyMS: 2000,
			TaskSource: "header", TaskExternalID: "run-1", Agent: "agent", ConfigLabel: "cfg",
		})
	}
	if err := st.InsertRequests(ctx, rows[:2]); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertRequests(ctx, rows[2:]); err != nil {
		t.Fatal(err)
	}
	got, calls, err := st.TaskWithCalls(ctx, "t_abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != store.OutcomeSolved || got.Calls != 3 || got.Agent != "agent" || got.ConfigLabel != "cfg" {
		t.Errorf("task = %+v", got)
	}
	if !got.StartedAt.Equal(start) || got.WallSeconds != 22 {
		t.Errorf("started %v (want %v), wall %v s (want 22: first call to the end of the last)", got.StartedAt, start, got.WallSeconds)
	}
	if got.MaxContextTokens != 2300 || got.FirstContextTokens != 100 || got.Cost != metering.FromUSD(0.03) {
		t.Errorf("context first %d max %d, cost %v", got.FirstContextTokens, got.MaxContextTokens, got.Cost)
	}
	if got.RepeatedInputTokens != 2700 || got.PromptTokens != 3600 {
		t.Errorf("repeated %d of %d prompt tokens", got.RepeatedInputTokens, got.PromptTokens)
	}
	if len(calls) != 3 || calls[2].ContextTokens != 2300 {
		t.Errorf("calls = %+v", calls)
	}
}
