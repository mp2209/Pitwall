package metering

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Budget enforces monthly spending limits with reserve-then-settle in Redis.
//
// Before a call, Reserve adds the call's worst-case cost to every scope it
// counts against (the key and its team), in one Lua script, and refuses if
// any scope's spent + reserved + worst case would pass its limit. After the
// call, Settle releases the reservation and adds the real cost. Because the
// check and the reservation are one atomic step, concurrent calls cannot
// together overrun a limit: ten calls arriving at once for a key with $1 left
// are admitted only while their reservations fit, and the rest are refused.
//
// Counters live in one Redis hash per scope and month, so they reset when
// the month changes. If a hash is missing (first call of the month, or Redis
// lost its data), the Seeder recomputes spend from the request log before
// the reservation goes ahead.
type Budget struct {
	rdb  redis.Cmdable
	seed Seeder
	now  func() time.Time
	ttl  time.Duration
}

// Seeder returns a scope's spend since a time, from durable storage.
type Seeder func(ctx context.Context, scopeID string, since time.Time) (Nanos, error)

// Scope is one counter a call is charged against.
type Scope struct {
	ID    string // stable identifier, for example "key:12" or "team:3"
	Label string // for error messages, for example "key pw-ab12cd34"
	Limit Nanos  // 0 means tracked but unlimited
}

// Reservation is money held for one call until it settles.
type Reservation struct {
	keys   []string
	Amount Nanos
}

// ExceededError says which scope a call would push past its limit.
type ExceededError struct {
	Scope    Scope
	Spent    Nanos
	Reserved Nanos
	Need     Nanos
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("monthly budget for %s exhausted: %s spent and %s held for calls in flight, of a %s limit; this call could cost up to %s",
		e.Scope.Label, e.Spent, e.Reserved, e.Scope.Limit, e.Need)
}

// NewBudget returns a Budget over a Redis client. seed may be nil, in which
// case a missing counter starts at zero.
func NewBudget(rdb redis.Cmdable, seed Seeder) *Budget {
	return &Budget{rdb: rdb, seed: seed, now: time.Now, ttl: 45 * 24 * time.Hour}
}

// MonthStart is the start of the UTC month containing t, when budgets reset.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// redisKey names a scope's counter for a month. The braces are a Redis
// Cluster hash tag: every budget counter lands in one slot, which the
// multi-key scripts need.
func redisKey(scopeID string, month time.Time) string {
	return "pitwall:{budget}:" + month.Format("2006-01") + ":" + scopeID
}

// reserveScript returns {1} when reserved, {-1, i} when KEYS[i] needs
// seeding, or {0, i, spent, reserved} when KEYS[i] would pass its limit.
var reserveScript = redis.NewScript(`
local amount = tonumber(ARGV[1])
for i, k in ipairs(KEYS) do
  if redis.call('EXISTS', k) == 0 then return {-1, i} end
end
for i, k in ipairs(KEYS) do
  local limit = tonumber(ARGV[2 + i])
  if limit > 0 then
    local spent = tonumber(redis.call('HGET', k, 'spent') or '0')
    local reserved = tonumber(redis.call('HGET', k, 'reserved') or '0')
    if spent + reserved + amount > limit then return {0, i, spent, reserved} end
  end
end
for _, k in ipairs(KEYS) do
  redis.call('HINCRBY', k, 'reserved', amount)
  redis.call('EXPIRE', k, ARGV[2])
end
return {1}
`)

// settleScript releases a reservation and adds the real cost.
var settleScript = redis.NewScript(`
local held = tonumber(ARGV[1])
local actual = tonumber(ARGV[2])
for _, k in ipairs(KEYS) do
  if held > 0 then
    local left = redis.call('HINCRBY', k, 'reserved', -held)
    if left < 0 then redis.call('HSET', k, 'reserved', 0) end
  end
  if actual > 0 then redis.call('HINCRBY', k, 'spent', actual) end
  redis.call('EXPIRE', k, ARGV[3])
end
return 1
`)

// Reserve holds amount against every scope, or returns an *ExceededError.
func (b *Budget) Reserve(ctx context.Context, scopes []Scope, amount Nanos) (*Reservation, error) {
	month := MonthStart(b.now())
	keys := make([]string, len(scopes))
	args := []any{int64(amount), int64(b.ttl.Seconds())}
	for i, s := range scopes {
		keys[i] = redisKey(s.ID, month)
		args = append(args, int64(s.Limit))
	}
	// Each pass seeds at most one missing counter, so len(scopes)+1 passes
	// always suffice.
	for range len(scopes) + 1 {
		res, err := reserveScript.Run(ctx, b.rdb, keys, args...).Int64Slice()
		if err != nil {
			return nil, fmt.Errorf("budget reserve: %w", err)
		}
		switch res[0] {
		case 1:
			return &Reservation{keys: keys, Amount: amount}, nil
		case -1:
			i := res[1] - 1
			if err := b.seedKey(ctx, scopes[i].ID, keys[i], month); err != nil {
				return nil, err
			}
		case 0:
			i := res[1] - 1
			return nil, &ExceededError{Scope: scopes[i], Spent: Nanos(res[2]), Reserved: Nanos(res[3]), Need: amount}
		}
	}
	return nil, errors.New("budget reserve: counters could not be seeded")
}

func (b *Budget) seedKey(ctx context.Context, scopeID, key string, month time.Time) error {
	var spent Nanos
	if b.seed != nil {
		var err error
		if spent, err = b.seed(ctx, scopeID, month); err != nil {
			return fmt.Errorf("budget seed %s: %w", scopeID, err)
		}
	}
	// HSETNX: if another gateway replica seeded first, keep its value.
	pipe := b.rdb.TxPipeline()
	pipe.HSetNX(ctx, key, "spent", int64(spent))
	pipe.HSetNX(ctx, key, "reserved", 0)
	pipe.Expire(ctx, key, b.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("budget seed %s: %w", scopeID, err)
	}
	return nil
}

// Unreserved returns a reservation holding nothing, used when Reserve could
// not reach Redis and the gateway was configured to let the call through:
// settling it still records the spend.
func (b *Budget) Unreserved(scopes []Scope) *Reservation {
	month := MonthStart(b.now())
	keys := make([]string, len(scopes))
	for i, s := range scopes {
		keys[i] = redisKey(s.ID, month)
	}
	return &Reservation{keys: keys}
}

// Settle releases a reservation and charges the real cost.
func (b *Budget) Settle(ctx context.Context, r *Reservation, actual Nanos) error {
	if r == nil || len(r.keys) == 0 {
		return nil
	}
	err := settleScript.Run(ctx, b.rdb, r.keys, int64(r.Amount), int64(actual), int64(b.ttl.Seconds())).Err()
	if err != nil {
		return fmt.Errorf("budget settle: %w", err)
	}
	return nil
}

// Status reports a scope's spend and holds for the current month, seeding the
// counter first if it is missing.
func (b *Budget) Status(ctx context.Context, scopeID string) (spent, reserved Nanos, err error) {
	month := MonthStart(b.now())
	key := redisKey(scopeID, month)
	n, err := b.rdb.Exists(ctx, key).Result()
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		if err := b.seedKey(ctx, scopeID, key, month); err != nil {
			return 0, 0, err
		}
	}
	vals, err := b.rdb.HMGet(ctx, key, "spent", "reserved").Result()
	if err != nil {
		return 0, 0, err
	}
	return parseNanos(vals[0]), parseNanos(vals[1]), nil
}

func parseNanos(v any) Nanos {
	s, _ := v.(string)
	n, _ := strconv.ParseInt(s, 10, 64)
	return Nanos(n)
}
