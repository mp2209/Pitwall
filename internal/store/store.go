// Package store is Pitwall's Postgres layer: schema migrations, teams, users,
// virtual keys, prices and the per-call request log.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/metering"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Errors the admin API turns into HTTP statuses.
var (
	ErrNotFound = auth.ErrNotFound
	ErrConflict = errors.New("already exists")
	ErrBadRef   = errors.New("refers to a row that does not exist")
)

// Store wraps a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrationLock is an arbitrary advisory-lock id, so that several gateway
// replicas starting at once apply each migration exactly once.
const migrationLock = 7_346_201

// Migrate applies embedded migrations that have not run yet, each in its own
// transaction, in file-name order.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var applied bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", version, err)
		}
	}
	return nil
}

// mapErr turns constraint violations into the package's errors.
func mapErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("%w: %s", ErrConflict, pg.Detail)
		case "23503":
			return fmt.Errorf("%w: %s", ErrBadRef, pg.Detail)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// --- teams and users ---------------------------------------------------------

type Team struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	MonthlyBudgetUSD float64   `json:"monthly_budget_usd"`
	CreatedAt        time.Time `json:"created_at"`
}

func (s *Store) CreateTeam(ctx context.Context, name string, budgetUSD float64) (Team, error) {
	t := Team{Name: name, MonthlyBudgetUSD: budgetUSD}
	err := s.pool.QueryRow(ctx,
		"INSERT INTO teams (name, monthly_budget_usd) VALUES ($1, $2) RETURNING id, created_at",
		name, budgetUSD).Scan(&t.ID, &t.CreatedAt)
	return t, mapErr(err)
}

// UpdateTeamBudget changes a team's monthly budget.
func (s *Store) UpdateTeamBudget(ctx context.Context, id int64, budgetUSD float64) (Team, error) {
	var t Team
	err := s.pool.QueryRow(ctx,
		"UPDATE teams SET monthly_budget_usd = $2 WHERE id = $1 RETURNING id, name, monthly_budget_usd::float8, created_at",
		id, budgetUSD).Scan(&t.ID, &t.Name, &t.MonthlyBudgetUSD, &t.CreatedAt)
	return t, mapErr(err)
}

func (s *Store) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, name, monthly_budget_usd::float8, created_at FROM teams ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Team, error) {
		var t Team
		return t, r.Scan(&t.ID, &t.Name, &t.MonthlyBudgetUSD, &t.CreatedAt)
	})
}

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	TeamID    *int64    `json:"team_id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) CreateUser(ctx context.Context, email string, teamID *int64, role string) (User, error) {
	u := User{Email: email, TeamID: teamID, Role: role}
	err := s.pool.QueryRow(ctx,
		"INSERT INTO users (email, team_id, role) VALUES ($1, $2, $3) RETURNING id, created_at",
		email, teamID, role).Scan(&u.ID, &u.CreatedAt)
	return u, mapErr(err)
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, "SELECT id, email, team_id, role, created_at FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (User, error) {
		var u User
		return u, r.Scan(&u.ID, &u.Email, &u.TeamID, &u.Role, &u.CreatedAt)
	})
}

// --- keys --------------------------------------------------------------------

// KeyInfo is a key as the admin API shows it: never the plaintext.
type KeyInfo struct {
	ID               int64      `json:"id"`
	UserID           int64      `json:"user_id"`
	UserEmail        string     `json:"user_email"`
	TeamID           *int64     `json:"team_id"`
	Name             string     `json:"name"`
	Prefix           string     `json:"prefix"`
	AllowedModels    []string   `json:"allowed_models"`
	MonthlyBudgetUSD float64    `json:"monthly_budget_usd"`
	CacheOptimizerOn bool       `json:"cache_optimizer_on"`
	CreatedAt        time.Time  `json:"created_at"`
	RevokedAt        *time.Time `json:"revoked_at"`
}

const keyInfoSelect = `SELECT k.id, k.user_id, u.email, u.team_id, k.name, k.key_prefix, k.allowed_models,
	k.monthly_budget_usd::float8, k.cache_optimizer_on, k.created_at, k.revoked_at
	FROM api_keys k JOIN users u ON u.id = k.user_id`

func scanKeyInfo(r pgx.Row) (KeyInfo, error) {
	var k KeyInfo
	err := r.Scan(&k.ID, &k.UserID, &k.UserEmail, &k.TeamID, &k.Name, &k.Prefix, &k.AllowedModels,
		&k.MonthlyBudgetUSD, &k.CacheOptimizerOn, &k.CreatedAt, &k.RevokedAt)
	if k.AllowedModels == nil {
		k.AllowedModels = []string{}
	}
	return k, err
}

// NewKey describes a key to create.
type NewKey struct {
	UserID           int64    `json:"user_id"`
	Name             string   `json:"name"`
	AllowedModels    []string `json:"allowed_models"`
	MonthlyBudgetUSD float64  `json:"monthly_budget_usd"`
	CacheOptimizerOn bool     `json:"cache_optimizer_on"`
}

// CreateKey stores a new key and returns its plaintext, which is not kept.
func (s *Store) CreateKey(ctx context.Context, nk NewKey) (KeyInfo, string, error) {
	plain, prefix, hash, err := auth.GenerateKey()
	if err != nil {
		return KeyInfo{}, "", err
	}
	if nk.AllowedModels == nil {
		nk.AllowedModels = []string{}
	}
	var id int64
	err = s.pool.QueryRow(ctx, `INSERT INTO api_keys
		(user_id, name, key_hash, key_prefix, allowed_models, monthly_budget_usd, cache_optimizer_on)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		nk.UserID, nk.Name, hash, prefix, nk.AllowedModels, nk.MonthlyBudgetUSD, nk.CacheOptimizerOn).Scan(&id)
	if err != nil {
		return KeyInfo{}, "", mapErr(err)
	}
	k, err := s.Key(ctx, id)
	return k, plain, err
}

// Key returns one key.
func (s *Store) Key(ctx context.Context, id int64) (KeyInfo, error) {
	k, err := scanKeyInfo(s.pool.QueryRow(ctx, keyInfoSelect+" WHERE k.id = $1", id))
	return k, mapErr(err)
}

func (s *Store) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := s.pool.Query(ctx, keyInfoSelect+" ORDER BY k.id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (KeyInfo, error) { return scanKeyInfo(r) })
}

// KeyUpdate changes the fields that are set.
type KeyUpdate struct {
	Name             *string   `json:"name"`
	AllowedModels    *[]string `json:"allowed_models"`
	MonthlyBudgetUSD *float64  `json:"monthly_budget_usd"`
	CacheOptimizerOn *bool     `json:"cache_optimizer_on"`
}

func (s *Store) UpdateKey(ctx context.Context, id int64, u KeyUpdate) (KeyInfo, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET
		name = coalesce($2, name),
		allowed_models = coalesce($3, allowed_models),
		monthly_budget_usd = coalesce($4, monthly_budget_usd),
		cache_optimizer_on = coalesce($5, cache_optimizer_on)
		WHERE id = $1`, id, u.Name, u.AllowedModels, u.MonthlyBudgetUSD, u.CacheOptimizerOn)
	if err != nil {
		return KeyInfo{}, mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return KeyInfo{}, ErrNotFound
	}
	return s.Key(ctx, id)
}

// RevokeKey marks a key revoked. Revoking twice keeps the first time.
func (s *Store) RevokeKey(ctx context.Context, id int64) (KeyInfo, error) {
	tag, err := s.pool.Exec(ctx, "UPDATE api_keys SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1", id)
	if err != nil {
		return KeyInfo{}, err
	}
	if tag.RowsAffected() == 0 {
		return KeyInfo{}, ErrNotFound
	}
	return s.Key(ctx, id)
}

// KeyByHash implements auth.Store.
func (s *Store) KeyByHash(ctx context.Context, hash []byte) (*auth.Key, error) {
	var (
		k                     auth.Key
		keyBudget, teamBudget float64
	)
	err := s.pool.QueryRow(ctx, `SELECT k.id, k.name, k.key_prefix, k.allowed_models,
			k.monthly_budget_usd::float8, k.cache_optimizer_on, k.revoked_at IS NOT NULL,
			u.id, u.email, u.role,
			coalesce(t.id, 0), coalesce(t.name, ''), coalesce(t.monthly_budget_usd, 0)::float8
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		LEFT JOIN teams t ON t.id = u.team_id
		WHERE k.key_hash = $1`, hash).Scan(
		&k.ID, &k.Name, &k.Prefix, &k.AllowedModels, &keyBudget, &k.CacheOptimizerOn, &k.Revoked,
		&k.UserID, &k.UserEmail, &k.Role, &k.TeamID, &k.TeamName, &teamBudget)
	if err != nil {
		return nil, mapErr(err)
	}
	k.MonthlyBudget, k.TeamBudget = metering.FromUSD(keyBudget), metering.FromUSD(teamBudget)
	return &k, nil
}

// --- prices ------------------------------------------------------------------

// SyncPrices inserts price rows that are not in the table yet. It never
// changes an existing row, so editing a number in the file has no effect
// until a new effective_from is added: recorded costs stay as billed.
func (s *Store) SyncPrices(ctx context.Context, prices []metering.Price) (inserted int, err error) {
	for _, p := range prices {
		tag, err := s.insertPrice(ctx, p, true)
		if err != nil {
			return inserted, fmt.Errorf("price %s: %w", p.Model, err)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// AddPrice inserts one price row, failing if the same model, speed and
// effective_from already exist.
func (s *Store) AddPrice(ctx context.Context, p metering.Price) error {
	_, err := s.insertPrice(ctx, p, false)
	return mapErr(err)
}

func (s *Store) insertPrice(ctx context.Context, p metering.Price, ignoreExisting bool) (pgconn.CommandTag, error) {
	var long []byte
	if p.LongContext != nil {
		long, _ = json.Marshal(p.LongContext)
	}
	sql := `INSERT INTO model_prices (model, speed, effective_from, input_per_mtok, cache_write_per_mtok,
		cache_write_1h_per_mtok, cached_read_per_mtok, output_per_mtok, long_context_threshold, long_context, us_multiplier)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	if ignoreExisting {
		sql += " ON CONFLICT DO NOTHING"
	}
	return s.pool.Exec(ctx, sql, p.Model, p.Speed, p.EffectiveFrom.UTC(), p.Input, p.CacheWrite, p.CacheWrite1h,
		p.CachedRead, p.Output, p.LongContextThreshold, long, p.USMultiplier)
}

// ListPrices returns every price row.
func (s *Store) ListPrices(ctx context.Context) ([]metering.Price, error) {
	rows, err := s.pool.Query(ctx, `SELECT model, speed, effective_from, input_per_mtok::float8,
		cache_write_per_mtok::float8, cache_write_1h_per_mtok::float8, cached_read_per_mtok::float8,
		output_per_mtok::float8, long_context_threshold, long_context, us_multiplier::float8
		FROM model_prices ORDER BY model, speed, effective_from`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (metering.Price, error) {
		var (
			p    metering.Price
			long []byte
		)
		err := r.Scan(&p.Model, &p.Speed, &p.EffectiveFrom, &p.Input, &p.CacheWrite, &p.CacheWrite1h,
			&p.CachedRead, &p.Output, &p.LongContextThreshold, &long, &p.USMultiplier)
		if err == nil && len(long) > 0 {
			p.LongContext = &metering.Rates{}
			err = json.Unmarshal(long, p.LongContext)
		}
		return p, err
	})
}

// --- spend -------------------------------------------------------------------

// SpendSince sums recorded cost for a budget scope ("key:12" or "team:3").
// It implements metering.Seeder.
func (s *Store) SpendSince(ctx context.Context, scopeID string, since time.Time) (metering.Nanos, error) {
	kind, id, ok := strings.Cut(scopeID, ":")
	if !ok {
		return 0, fmt.Errorf("bad scope %q", scopeID)
	}
	var column string
	switch kind {
	case "key":
		column = "key_id"
	case "team":
		column = "team_id"
	default:
		return 0, fmt.Errorf("bad scope %q", scopeID)
	}
	var nanos int64
	err := s.pool.QueryRow(ctx,
		"SELECT coalesce(sum(cost_usd) * 1000000000, 0)::bigint FROM requests WHERE "+column+" = $1::bigint AND ts >= $2",
		id, since).Scan(&nanos)
	return metering.Nanos(nanos), err
}

// Exec runs a statement and discards its rows. Tests use it to check that
// queries written elsewhere (dashboards) still match the schema.
func (s *Store) Exec(ctx context.Context, sql string) error {
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}
