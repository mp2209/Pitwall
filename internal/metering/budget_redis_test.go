package metering

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestBudgetOnRealRedis runs the Lua scripts on a real Redis server
// (miniredis uses a different Lua interpreter). It uses random scope IDs and
// never flushes, so it is safe against a shared server. CI sets the URL.
func TestBudgetOnRealRedis(t *testing.T) {
	url := os.Getenv("PITWALL_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PITWALL_TEST_REDIS_URL not set; skipping real Redis test")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	var b [6]byte
	_, _ = rand.Read(b[:])
	id := "test-" + hex.EncodeToString(b[:])
	seeded := FromUSD(0.10)
	budget := NewBudget(rdb, func(context.Context, string, time.Time) (Nanos, error) { return seeded, nil })
	ctx := context.Background()
	key := Scope{ID: "key:" + id, Label: "key test", Limit: FromUSD(1)}
	team := Scope{ID: "team:" + id, Label: "team test", Limit: FromUSD(100)}
	t.Cleanup(func() {
		month := MonthStart(budget.now())
		rdb.Del(context.Background(), redisKey(key.ID, month), redisKey(team.ID, month))
	})

	// $0.10 already spent (seeded), $0.90 left: 3 of 10 calls at $0.30 fit.
	var wg sync.WaitGroup
	var admitted, refused atomic.Int64
	res := make(chan *Reservation, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := budget.Reserve(ctx, []Scope{key, team}, FromUSD(0.30))
			var ex *ExceededError
			switch {
			case err == nil:
				admitted.Add(1)
				res <- r
			case errors.As(err, &ex):
				refused.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(res)
	if admitted.Load() != 3 || refused.Load() != 7 {
		t.Fatalf("admitted %d refused %d, want 3/7", admitted.Load(), refused.Load())
	}
	for r := range res {
		if err := budget.Settle(ctx, r, FromUSD(0.05)); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []Scope{key, team} {
		spent, held, err := budget.Status(ctx, s.ID)
		if err != nil || spent != seeded+3*FromUSD(0.05) || held != 0 {
			t.Errorf("%s: spent %v held %v err %v", s.ID, spent, held, err)
		}
	}
}
