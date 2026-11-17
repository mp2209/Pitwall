package metering

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

var opus55 = Price{
	Model:        "claude-opus-5-5",
	Rates:        Rates{Input: 4, CacheWrite: 5, CacheWrite1h: 8, CachedRead: 0.20, Output: 20},
	USMultiplier: 1.1,
}

func TestCostFormula(t *testing.T) {
	u := Usage{InputTokens: 1000, CachedReadTokens: 100_000, CacheWriteTokens: 2000, CacheWrite1hTokens: 500, OutputTokens: 800}
	// 1000·4 + 100000·0.20 + 2000·5 + 500·8 + 800·20 = 4000 + 20000 + 10000 + 4000 + 16000
	// = 54000 USD-per-million-token units = $0.054.
	if got := opus55.Cost(u); got != FromUSD(0.054) {
		t.Errorf("cost = %v (%d nanos), want $0.054", got, got)
	}
	u.InferenceGeo = "us"
	if got := opus55.Cost(u); got != FromUSD(0.0594) {
		t.Errorf("US-only cost = %v, want 1.1x = $0.0594", got)
	}
	// One cached token on a cheap model must not round to zero.
	luna := Price{Rates: Rates{CachedRead: 0.01}}
	if got := luna.Cost(Usage{CachedReadTokens: 1}); got != 10 {
		t.Errorf("one cached token = %d nanos, want 10", got)
	}
}

func TestLongContextTier(t *testing.T) {
	haiku := Price{
		Model:                "claude-haiku-5-5",
		Rates:                Rates{Input: 0.10, CacheWrite: 0.125, CachedRead: 0.01, Output: 0.50},
		LongContextThreshold: 100_000,
		LongContext:          &Rates{Input: 0.50, CacheWrite: 0.625, CachedRead: 0.05, Output: 2.50},
	}
	short := Usage{InputTokens: 100_000, OutputTokens: 1000}
	if got := haiku.Cost(short); got != FromUSD(0.0105) {
		t.Errorf("100k prompt = %v, want standard rates $0.0105", got)
	}
	// Cache reads count toward the prompt length that picks the tier.
	long := Usage{InputTokens: 1000, CachedReadTokens: 100_000, OutputTokens: 1000}
	// 1000·0.50 + 100000·0.05 + 1000·2.50 = 500 + 5000 + 2500 = 8000 -> $0.008
	if got := haiku.Cost(long); got != FromUSD(0.008) {
		t.Errorf("101k prompt = %v, want long-context rates $0.008", got)
	}
}

func TestMaxCostIsAnUpperBound(t *testing.T) {
	worst := opus55.MaxCost(10_000, 4096)
	for _, u := range []Usage{
		{InputTokens: 10_000, OutputTokens: 4096},
		{CacheWrite1hTokens: 10_000, OutputTokens: 4096, InferenceGeo: "us"},
		{CachedReadTokens: 9000, CacheWriteTokens: 1000, OutputTokens: 4096, InferenceGeo: "us"},
	} {
		if c := opus55.Cost(u); c > worst {
			t.Errorf("cost %v exceeds MaxCost %v for %+v", c, worst, u)
		}
	}
}

func TestPriceBookLookup(t *testing.T) {
	jan := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jun := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	book := NewPriceBook([]Price{
		{Model: "claude-sonnet-4-5", Rates: Rates{Input: 3}},
		{Model: "gpt-x", Rates: Rates{Input: 2}, EffectiveFrom: jan},
		{Model: "gpt-x", Rates: Rates{Input: 1}, EffectiveFrom: jun},
		{Model: "claude-opus-5-5", Speed: "fast", Rates: Rates{Input: 8}},
		{Model: "claude-opus-5-5", Rates: Rates{Input: 4}},
		{Model: "mock-*", Rates: Rates{Input: 9}},
		{Model: "mock-claude*", Rates: Rates{Input: 7}},
	})
	cases := []struct {
		model, speed string
		at           time.Time
		want         float64
		found        bool
	}{
		{"claude-sonnet-4-5-20250929", "", jun, 3, true}, // snapshot date stripped
		{"gpt-x", "", jan.AddDate(0, 2, 0), 2, true},     // older row before the change
		{"gpt-x", "", jun.AddDate(0, 0, 1), 1, true},     // newer row after it
		{"gpt-x", "", jan.AddDate(0, 0, -1), 0, false},   // before any row: unpriced
		{"claude-opus-5-5", "fast", jun, 8, true},
		{"claude-opus-5-5", "", jun, 4, true},
		{"mock-claude-big", "", jun, 7, true}, // longest prefix wins
		{"mock-gpt", "", jun, 9, true},
		{"llama3", "", jun, 0, false},
	}
	for _, c := range cases {
		p, ok := book.Lookup(c.model, c.speed, c.at)
		if ok != c.found || p.Input != c.want {
			t.Errorf("Lookup(%s, %q, %s) = %v %v, want %v %v", c.model, c.speed, c.at.Format("2006-01-02"), p.Input, ok, c.want, c.found)
		}
	}
}

func TestCountTokens(t *testing.T) {
	if n := CountTokens("Hello, world!"); n != 4 {
		t.Errorf("o200k count of 'Hello, world!' = %d, want 4", n)
	}
	if CountTokens("") != 0 {
		t.Error("empty text should be 0 tokens")
	}
}

func newTestBudget(t *testing.T, seed Seeder) (*Budget, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	b := NewBudget(redis.NewClient(&redis.Options{Addr: mr.Addr()}), seed)
	b.now = func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }
	return b, mr
}

func TestReserveSettle(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBudget(t, nil)
	key := Scope{ID: "key:1", Label: "key pw-test", Limit: FromUSD(1)}

	r, err := b.Reserve(ctx, []Scope{key}, FromUSD(0.40))
	if err != nil {
		t.Fatal(err)
	}
	if spent, held, _ := b.Status(ctx, "key:1"); spent != 0 || held != FromUSD(0.40) {
		t.Errorf("after reserve: spent %v held %v", spent, held)
	}
	if err := b.Settle(ctx, r, FromUSD(0.05)); err != nil {
		t.Fatal(err)
	}
	if spent, held, _ := b.Status(ctx, "key:1"); spent != FromUSD(0.05) || held != 0 {
		t.Errorf("after settle: spent %v held %v", spent, held)
	}

	_, err = b.Reserve(ctx, []Scope{key}, FromUSD(0.96))
	var ex *ExceededError
	if !errors.As(err, &ex) || ex.Spent != FromUSD(0.05) || ex.Need != FromUSD(0.96) {
		t.Fatalf("want exceeded error, got %v", err)
	}
	if _, err := b.Reserve(ctx, []Scope{key}, FromUSD(0.95)); err != nil {
		t.Errorf("a call that exactly fits should pass: %v", err)
	}
}

// The plan's interview question: ten calls arrive at once for a key with $1
// left. Each reserves its worst case atomically, so exactly as many as fit
// get through, never more.
func TestConcurrentReservationsNeverOverrun(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBudget(t, nil)
	key := Scope{ID: "key:7", Label: "key pw-busy", Limit: FromUSD(1)}

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, refused := 0, 0
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Reserve(ctx, []Scope{key}, FromUSD(0.30))
			mu.Lock()
			defer mu.Unlock()
			var ex *ExceededError
			switch {
			case err == nil:
				admitted++
			case errors.As(err, &ex):
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if admitted != 3 || refused != 7 {
		t.Errorf("admitted %d refused %d, want 3 and 7", admitted, refused)
	}
	if _, held, _ := b.Status(ctx, "key:7"); held != FromUSD(0.90) {
		t.Errorf("held %v, want $0.90", held)
	}
}

func TestTeamLimitAppliesAcrossKeys(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBudget(t, nil)
	team := Scope{ID: "team:1", Label: "team core", Limit: FromUSD(1)}
	a := Scope{ID: "key:1", Label: "key a"} // unlimited keys, limited team
	c := Scope{ID: "key:2", Label: "key c"}

	r, err := b.Reserve(ctx, []Scope{a, team}, FromUSD(0.7))
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Settle(ctx, r, FromUSD(0.7))
	_, err = b.Reserve(ctx, []Scope{c, team}, FromUSD(0.5))
	var ex *ExceededError
	if !errors.As(err, &ex) || ex.Scope.ID != "team:1" {
		t.Fatalf("the team limit should stop key c: %v", err)
	}
	if spent, held, _ := b.Status(ctx, "key:2"); spent != 0 || held != 0 {
		t.Errorf("a refused call must not hold money on any scope: spent %v held %v", spent, held)
	}
}

func TestMissingCounterIsSeededFromTheLog(t *testing.T) {
	ctx := context.Background()
	var seededSince time.Time
	b, mr := newTestBudget(t, func(_ context.Context, scopeID string, since time.Time) (Nanos, error) {
		seededSince = since
		if scopeID == "key:3" {
			return FromUSD(0.90), nil // spent earlier this month, before Redis lost its data
		}
		return 0, nil
	})
	key := Scope{ID: "key:3", Label: "key pw-x", Limit: FromUSD(1)}
	if _, err := b.Reserve(ctx, []Scope{key}, FromUSD(0.20)); err == nil {
		t.Fatal("seeded spend of $0.90 + $0.20 should exceed $1")
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !seededSince.Equal(want) {
		t.Errorf("seeded since %v, want start of month %v", seededSince, want)
	}
	if !mr.Exists(redisKey("key:3", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))) {
		t.Error("counter should exist after seeding")
	}
}

func TestMonthRollover(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBudget(t, nil)
	key := Scope{ID: "key:4", Label: "key", Limit: FromUSD(1)}
	r, _ := b.Reserve(ctx, []Scope{key}, FromUSD(1))
	_ = b.Settle(ctx, r, FromUSD(1))
	if _, err := b.Reserve(ctx, []Scope{key}, FromUSD(0.01)); err == nil {
		t.Fatal("budget should be spent")
	}
	b.now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC) }
	if _, err := b.Reserve(ctx, []Scope{key}, FromUSD(0.01)); err != nil {
		t.Errorf("a new month should reset the budget: %v", err)
	}
}

func TestUnreservedSettleStillCharges(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBudget(t, nil)
	scopes := []Scope{{ID: "key:5"}}
	if err := b.Settle(ctx, b.Unreserved(scopes), FromUSD(0.25)); err != nil {
		t.Fatal(err)
	}
	if spent, _, _ := b.Status(ctx, "key:5"); spent != FromUSD(0.25) {
		t.Errorf("spent %v", spent)
	}
}
