// Package auth handles Pitwall's virtual keys: one key per developer, stored
// only as a hash, each mapping to a user, a team, allowed models and a
// monthly budget. Real provider keys never leave the gateway.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
)

// KeyPrefix starts every Pitwall key, so a leaked one is easy to recognize.
const KeyPrefix = "pw-"

// displayLen is how much of a key is kept in plain text for display and
// logs: "pw-" plus 8 characters, which identifies a key without exposing it.
const displayLen = len(KeyPrefix) + 8

// Roles a user can have.
const (
	RoleDev   = "dev"
	RoleAdmin = "admin"
)

var (
	// ErrInvalidKey means no active key matches.
	ErrInvalidKey = errors.New("invalid or missing Pitwall API key")
	// ErrRevokedKey means the key existed but was revoked.
	ErrRevokedKey = errors.New("this Pitwall API key has been revoked")
	// ErrNotFound is returned by a Store when no key has the hash.
	ErrNotFound = errors.New("not found")
)

// GenerateKey makes a new key. Only the hash and the display prefix should
// be stored; the plaintext is shown to the admin once.
func GenerateKey() (plaintext, prefix string, hash []byte, err error) {
	var b [24]byte // 192 bits
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", nil, err
	}
	plaintext = KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	return plaintext, plaintext[:displayLen], HashKey(plaintext), nil
}

// HashKey hashes a key for storage and lookup. Keys carry 192 random bits,
// so a fast hash is enough; a slow password hash would only add latency to
// every request.
func HashKey(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// Key is everything the gateway needs to know about a caller.
type Key struct {
	ID               int64
	Name             string
	Prefix           string
	UserID           int64
	UserEmail        string
	Role             string
	TeamID           int64 // 0 when the user has no team
	TeamName         string
	AllowedModels    []string // path.Match patterns; empty allows all
	MonthlyBudget    metering.Nanos
	TeamBudget       metering.Nanos
	CacheOptimizerOn bool
	Revoked          bool
}

// AllowsModel reports whether the key may call a model, by the name the
// client asked for.
func (k *Key) AllowsModel(model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, pattern := range k.AllowedModels {
		if ok, _ := path.Match(pattern, model); ok {
			return true
		}
	}
	return false
}

// Scopes lists the budget counters a call with this key is charged to.
func (k *Key) Scopes() []metering.Scope {
	scopes := []metering.Scope{{ID: KeyScope(k.ID), Label: "key " + k.Prefix, Limit: k.MonthlyBudget}}
	if k.TeamID != 0 {
		scopes = append(scopes, metering.Scope{ID: TeamScope(k.TeamID), Label: "team " + k.TeamName, Limit: k.TeamBudget})
	}
	return scopes
}

// KeyScope and TeamScope name budget counters.
func KeyScope(id int64) string  { return "key:" + strconv.FormatInt(id, 10) }
func TeamScope(id int64) string { return "team:" + strconv.FormatInt(id, 10) }

// Store looks keys up by hash.
type Store interface {
	KeyByHash(ctx context.Context, hash []byte) (*Key, error)
}

// Authenticator resolves keys with a short-lived cache, so most requests
// never touch the database. A revoked or changed key takes effect within the
// cache lifetime on every gateway replica, and at once on the replica that
// made the change (it calls Invalidate).
type Authenticator struct {
	store  Store
	ttl    time.Duration
	negTTL time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	key     *Key // nil for a known-bad key
	expires time.Time
}

const maxCacheEntries = 50_000

// NewAuthenticator caches good keys for ttl and unknown ones for a few
// seconds.
func NewAuthenticator(store Store, ttl time.Duration) *Authenticator {
	return &Authenticator{store: store, ttl: ttl, negTTL: 5 * time.Second, now: time.Now, cache: map[string]cacheEntry{}}
}

// Authenticate returns the key for a plaintext, ErrInvalidKey or
// ErrRevokedKey, or a store error.
func (a *Authenticator) Authenticate(ctx context.Context, plaintext string) (*Key, error) {
	if !strings.HasPrefix(plaintext, KeyPrefix) {
		return nil, ErrInvalidKey
	}
	hash := string(HashKey(plaintext))
	now := a.now()
	a.mu.Lock()
	e, ok := a.cache[hash]
	a.mu.Unlock()
	if !ok || now.After(e.expires) {
		k, err := a.store.KeyByHash(ctx, []byte(hash))
		switch {
		case errors.Is(err, ErrNotFound):
			e = cacheEntry{expires: now.Add(a.negTTL)}
		case err != nil:
			return nil, err
		default:
			e = cacheEntry{key: k, expires: now.Add(a.ttl)}
		}
		a.mu.Lock()
		if len(a.cache) >= maxCacheEntries {
			a.cache = map[string]cacheEntry{}
		}
		a.cache[hash] = e
		a.mu.Unlock()
	}
	switch {
	case e.key == nil:
		return nil, ErrInvalidKey
	case e.key.Revoked:
		return nil, ErrRevokedKey
	}
	return e.key, nil
}

// Invalidate drops all cached keys.
func (a *Authenticator) Invalidate() {
	a.mu.Lock()
	a.cache = map[string]cacheEntry{}
	a.mu.Unlock()
}
