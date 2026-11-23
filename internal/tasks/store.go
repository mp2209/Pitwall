package tasks

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore keeps task state in Redis, shared by every gateway replica.
// Updates use WATCH/MULTI, retried when another replica wins the race.
type RedisStore struct {
	rdb redis.UniversalClient
}

func NewRedisStore(rdb redis.UniversalClient) *RedisStore { return &RedisStore{rdb: rdb} }

func (s *RedisStore) Update(ctx context.Context, key string, ttl time.Duration, fn func([]byte) ([]byte, error)) error {
	for attempt := range 30 {
		if attempt > 0 {
			// Jittered backoff, so racing writers stop colliding.
			wait := time.Duration(rand.Int64N(int64(time.Millisecond) << min(attempt, 5)))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			old, err := tx.Get(ctx, key).Bytes()
			if err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
			val, err := fn(old)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, key, val, ttl)
				return nil
			})
			return err
		}, key)
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return err
	}
	return errors.New("task state: gave up after repeated concurrent updates")
}

func (s *RedisStore) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return v, err
}

// MemStore keeps task state in memory, for a single gateway without Redis.
type MemStore struct {
	mu      sync.Mutex
	entries map[string]memEntry
	now     func() time.Time
}

type memEntry struct {
	val     []byte
	expires time.Time
}

func NewMemStore() *MemStore { return &MemStore{entries: map[string]memEntry{}, now: time.Now} }

const maxMemEntries = 100_000

func (s *MemStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && s.now().Before(e.expires) {
		return e.val, nil
	}
	return nil, nil
}

func (s *MemStore) Update(_ context.Context, key string, ttl time.Duration, fn func([]byte) ([]byte, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var old []byte
	if e, ok := s.entries[key]; ok && now.Before(e.expires) {
		old = e.val
	}
	val, err := fn(old)
	if err != nil {
		return err
	}
	if len(s.entries) >= maxMemEntries {
		for k, e := range s.entries {
			if !now.Before(e.expires) {
				delete(s.entries, k)
			}
		}
		if len(s.entries) >= maxMemEntries {
			s.entries = map[string]memEntry{}
		}
	}
	s.entries[key] = memEntry{val: val, expires: now.Add(ttl)}
	return nil
}
