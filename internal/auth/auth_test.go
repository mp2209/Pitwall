package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeStore struct {
	keys  map[string]*Key
	calls int
	err   error
}

func (f *fakeStore) KeyByHash(_ context.Context, hash []byte) (*Key, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if k, ok := f.keys[string(hash)]; ok {
		return k, nil
	}
	return nil, ErrNotFound
}

func TestGenerateKey(t *testing.T) {
	a, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, _ := GenerateKey()
	if a == b {
		t.Fatal("keys must be random")
	}
	if !strings.HasPrefix(a, "pw-") || len(a) != 35 || prefix != a[:11] {
		t.Errorf("key %q prefix %q", a, prefix)
	}
	if !bytes.Equal(hash, HashKey(a)) || len(hash) != 32 {
		t.Error("hash mismatch")
	}
}

func TestAuthenticatorCachesAndHonorsRevocation(t *testing.T) {
	plain, _, hash, _ := GenerateKey()
	key := &Key{ID: 1, Prefix: plain[:11]}
	st := &fakeStore{keys: map[string]*Key{string(hash): key}}
	a := NewAuthenticator(st, time.Minute)
	now := time.Now()
	a.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		if k, err := a.Authenticate(ctx, plain); err != nil || k.ID != 1 {
			t.Fatalf("got %v %v", k, err)
		}
	}
	if st.calls != 1 {
		t.Errorf("store called %d times, want 1 (cached)", st.calls)
	}

	key2 := *key
	key2.Revoked = true
	st.keys[string(hash)] = &key2
	if _, err := a.Authenticate(ctx, plain); err != nil {
		t.Error("revocation should not apply before the cache entry expires")
	}
	a.Invalidate()
	if _, err := a.Authenticate(ctx, plain); !errors.Is(err, ErrRevokedKey) {
		t.Errorf("after invalidate: %v, want revoked", err)
	}
}

func TestAuthenticatorRejectsUnknownAndMalformedKeys(t *testing.T) {
	st := &fakeStore{keys: map[string]*Key{}}
	a := NewAuthenticator(st, time.Minute)
	ctx := context.Background()
	if _, err := a.Authenticate(ctx, "sk-not-ours"); !errors.Is(err, ErrInvalidKey) || st.calls != 0 {
		t.Errorf("malformed key should fail without a lookup: %v, %d calls", err, st.calls)
	}
	for range 2 {
		if _, err := a.Authenticate(ctx, "pw-unknown"); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("unknown key: %v", err)
		}
	}
	if st.calls != 1 {
		t.Errorf("unknown keys should be cached briefly: %d calls", st.calls)
	}
	st.err = errors.New("db down")
	a.Invalidate()
	if _, err := a.Authenticate(ctx, "pw-unknown"); err == nil || errors.Is(err, ErrInvalidKey) {
		t.Errorf("a store failure must not look like a bad key: %v", err)
	}
}

func TestAllowsModelAndScopes(t *testing.T) {
	k := &Key{ID: 4, Prefix: "pw-abc", AllowedModels: []string{"claude-*", "gpt-5.4-mini"}, TeamID: 2, TeamName: "core"}
	for model, want := range map[string]bool{"claude-opus-5-5": true, "gpt-5.4-mini": true, "gpt-5.5": false} {
		if k.AllowsModel(model) != want {
			t.Errorf("AllowsModel(%s) != %v", model, want)
		}
	}
	if !(&Key{}).AllowsModel("anything") {
		t.Error("an empty allow-list allows all")
	}
	s := k.Scopes()
	if len(s) != 2 || s[0].ID != "key:4" || s[1].ID != "team:2" || s[1].Label != "team core" {
		t.Errorf("scopes = %+v", s)
	}
}
