// Package storetest gives tests a migrated, private Postgres schema.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mp2209/pitwall/internal/store"
)

// EnvVar names the database tests run against. Tests that need it are
// skipped when it is unset; CI sets it.
const EnvVar = "PITWALL_TEST_DATABASE_URL"

// New returns a Store on a fresh schema, dropped when the test ends.
func New(t testing.TB) *store.Store {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skipf("%s not set; skipping Postgres test", EnvVar)
	}
	ctx := context.Background()
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "t_" + hex.EncodeToString(b[:])

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect %s: %v", EnvVar, err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	st, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}
