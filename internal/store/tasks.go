package store

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mp2209/pitwall/internal/metering"
)

// Task outcomes.
const (
	OutcomeSolved  = "solved"
	OutcomeFailed  = "failed"
	OutcomeStopped = "stopped"
	OutcomeUnknown = "unknown"
)

// ValidOutcome reports whether s is an outcome a client may set.
func ValidOutcome(s string) bool {
	return s == OutcomeSolved || s == OutcomeFailed || s == OutcomeStopped || s == OutcomeUnknown
}

// Task is one agent run with its totals.
type Task struct {
	ID                  string         `json:"id"`
	KeyID               int64          `json:"key_id"`
	TeamID              *int64         `json:"team_id"`
	Source              string         `json:"source"`
	ExternalID          *string        `json:"external_id"`
	Agent               string         `json:"agent"`
	ConfigLabel         string         `json:"config_label"`
	StartedAt           time.Time      `json:"started_at"`
	EndedAt             time.Time      `json:"ended_at"`
	WallSeconds         float64        `json:"wall_seconds"`
	Calls               int            `json:"calls"`
	FailedCalls         int            `json:"failed_calls"`
	InputTokens         int64          `json:"input_tokens"`
	CachedReadTokens    int64          `json:"cached_read_tokens"`
	CacheWriteTokens    int64          `json:"cache_write_tokens"`
	OutputTokens        int64          `json:"output_tokens"`
	PromptTokens        int64          `json:"prompt_tokens"`
	RepeatedInputTokens int64          `json:"repeated_input_tokens"`
	RepeatedShare       float64        `json:"repeated_share"`
	FirstContextTokens  int64          `json:"first_context_tokens"`
	MaxContextTokens    int64          `json:"max_context_tokens"`
	Cost                metering.Nanos `json:"-"`
	CostUSD             float64        `json:"cost_usd"`
	Estimated           bool           `json:"estimated"`
	Outcome             string         `json:"outcome"`
	GuardEvents         int            `json:"guard_events"`
}

// taskDelta is what one batch of calls adds to a task.
type taskDelta struct {
	first                RequestRow
	start, end           time.Time
	calls, failed        int
	in, cr, cw, out, rep int64
	prompt, maxContext   int64
	cost                 metering.Nanos
	estimated            bool
	guardEvents          int
	stopped              bool
}

const upsertTaskSQL = `INSERT INTO tasks (id, key_id, team_id, source, external_id, agent, config_label,
	started_at, ended_at, calls, failed_calls, input_tokens, cached_read_tokens, cache_write_tokens,
	output_tokens, prompt_tokens, repeated_input_tokens, max_context_tokens, cost_usd, estimated,
	guard_events, outcome)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
ON CONFLICT (id) DO UPDATE SET
	-- A row with no calls yet is a stub made by an early outcome report.
	started_at = CASE WHEN tasks.calls = 0 THEN excluded.started_at ELSE least(tasks.started_at, excluded.started_at) END,
	ended_at = CASE WHEN tasks.calls = 0 THEN excluded.ended_at ELSE greatest(tasks.ended_at, excluded.ended_at) END,
	calls = tasks.calls + excluded.calls,
	failed_calls = tasks.failed_calls + excluded.failed_calls,
	input_tokens = tasks.input_tokens + excluded.input_tokens,
	cached_read_tokens = tasks.cached_read_tokens + excluded.cached_read_tokens,
	cache_write_tokens = tasks.cache_write_tokens + excluded.cache_write_tokens,
	output_tokens = tasks.output_tokens + excluded.output_tokens,
	prompt_tokens = tasks.prompt_tokens + excluded.prompt_tokens,
	repeated_input_tokens = tasks.repeated_input_tokens + excluded.repeated_input_tokens,
	max_context_tokens = greatest(tasks.max_context_tokens, excluded.max_context_tokens),
	cost_usd = tasks.cost_usd + excluded.cost_usd,
	estimated = tasks.estimated OR excluded.estimated,
	guard_events = tasks.guard_events + excluded.guard_events,
	-- The guard stopping a call marks the task stopped unless an outcome
	-- was already reported.
	outcome = CASE WHEN excluded.outcome = 'stopped' AND tasks.outcome = 'unknown' THEN 'stopped' ELSE tasks.outcome END,
	external_id = coalesce(tasks.external_id, excluded.external_id),
	agent = CASE WHEN tasks.agent = '' THEN excluded.agent ELSE tasks.agent END,
	config_label = CASE WHEN tasks.config_label = '' THEN excluded.config_label ELSE tasks.config_label END`

// upsertTasks folds a batch of calls into their tasks' rows.
func upsertTasks(ctx context.Context, tx pgx.Tx, rows []RequestRow) error {
	deltas := map[string]*taskDelta{}
	for _, r := range rows {
		if r.TaskID == nil {
			continue
		}
		d, ok := deltas[*r.TaskID]
		end := r.Time.Add(time.Duration(r.LatencyMS) * time.Millisecond)
		if !ok {
			d = &taskDelta{first: r, start: r.Time, end: end}
			deltas[*r.TaskID] = d
		}
		if r.Time.Before(d.start) {
			d.start = r.Time
		}
		if end.After(d.end) {
			d.end = end
		}
		d.calls++
		if r.Status >= 400 {
			d.failed++
		}
		d.in += r.InputTokens
		d.cr += r.CachedReadTokens
		d.cw += r.CacheWriteTokens + r.CacheWrite1hTokens
		d.out += r.OutputTokens
		d.rep += r.RepeatedInputTokens
		d.prompt += r.PromptTokens()
		d.maxContext = max(d.maxContext, r.PromptTokens())
		d.cost += r.Cost
		d.estimated = d.estimated || r.Estimated
		if r.GuardAction != "" {
			d.guardEvents++
		}
		d.stopped = d.stopped || r.GuardAction == "stop"
	}
	if len(deltas) == 0 {
		return nil
	}
	// Update tasks in a fixed order so concurrent gateway replicas cannot
	// deadlock on the same rows.
	ids := make([]string, 0, len(deltas))
	for id := range deltas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	batch := &pgx.Batch{}
	for _, id := range ids {
		d := deltas[id]
		var external *string
		if d.first.TaskExternalID != "" {
			external = &d.first.TaskExternalID
		}
		outcome := OutcomeUnknown
		if d.stopped {
			outcome = OutcomeStopped
		}
		batch.Queue(upsertTaskSQL, id, d.first.KeyID, d.first.TeamID, d.first.TaskSource, external,
			d.first.Agent, d.first.ConfigLabel, d.start, d.end, d.calls, d.failed, d.in, d.cr, d.cw, d.out,
			d.prompt, d.rep, d.maxContext, usdNumeric(d.cost), d.estimated, d.guardEvents, outcome)
	}
	return tx.SendBatch(ctx, batch).Close()
}

// TaskRef identifies a task an outcome is reported for.
type TaskRef struct {
	ID         string
	KeyID      int64
	TeamID     *int64
	Source     string
	ExternalID string
}

// SetTaskOutcome records how a task ended. The outcome may arrive before the
// task's last calls are written; it then creates a stub row that the calls
// fill in. A key can only set outcomes on its own tasks.
func (s *Store) SetTaskOutcome(ctx context.Context, ref TaskRef, outcome string) error {
	var external *string
	if ref.ExternalID != "" {
		external = &ref.ExternalID
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO tasks (id, key_id, team_id, source, external_id, started_at, ended_at, outcome)
		VALUES ($1, $2, $3, $4, $5, now(), now(), $6)
		ON CONFLICT (id) DO UPDATE SET outcome = excluded.outcome WHERE tasks.key_id = excluded.key_id
		RETURNING id`, ref.ID, ref.KeyID, ref.TeamID, ref.Source, external, outcome).Scan(&id)
	return mapErr(err)
}

// TaskFilter narrows ListTasks. Zero values match everything.
type TaskFilter struct {
	KeyID       int64
	ConfigLabel string
	Outcome     string
	Source      string
	Limit       int
}

const taskSelect = `SELECT t.id, t.key_id, t.team_id, t.source, t.external_id, t.agent, t.config_label,
	t.started_at, t.ended_at, t.calls, t.failed_calls, t.input_tokens, t.cached_read_tokens,
	t.cache_write_tokens, t.output_tokens, t.prompt_tokens, t.repeated_input_tokens, t.max_context_tokens,
	(t.cost_usd * 1000000000)::bigint, t.estimated, t.outcome, t.guard_events,
	coalesce((SELECT r.input_tokens + r.cached_read_tokens + r.cache_write_tokens + r.cache_write_1h_tokens
		FROM requests r WHERE r.task_id = t.id ORDER BY r.ts LIMIT 1), 0)
	FROM tasks t`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.KeyID, &t.TeamID, &t.Source, &t.ExternalID, &t.Agent, &t.ConfigLabel,
		&t.StartedAt, &t.EndedAt, &t.Calls, &t.FailedCalls, &t.InputTokens, &t.CachedReadTokens,
		&t.CacheWriteTokens, &t.OutputTokens, &t.PromptTokens, &t.RepeatedInputTokens, &t.MaxContextTokens,
		&t.Cost, &t.Estimated, &t.Outcome, &t.GuardEvents, &t.FirstContextTokens)
	t.CostUSD = t.Cost.USD()
	if t.Calls > 0 {
		t.WallSeconds = t.EndedAt.Sub(t.StartedAt).Seconds()
	}
	if t.PromptTokens > 0 {
		t.RepeatedShare = float64(t.RepeatedInputTokens) / float64(t.PromptTokens)
	}
	return t, err
}

// ListTasks returns tasks, newest first.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	rows, err := s.pool.Query(ctx, taskSelect+`
		WHERE ($1 = 0 OR t.key_id = $1) AND ($2 = '' OR t.config_label = $2)
		  AND ($3 = '' OR t.outcome = $3) AND ($4 = '' OR t.source = $4)
		ORDER BY t.started_at DESC LIMIT $5`, f.KeyID, f.ConfigLabel, f.Outcome, f.Source, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Task, error) { return scanTask(r) })
}

// TaskCall is one call of a task, for the per-turn view of context growth.
type TaskCall struct {
	ID                  string    `json:"id"`
	Time                time.Time `json:"ts"`
	Model               string    `json:"model"`
	Status              int       `json:"status"`
	ContextTokens       int64     `json:"context_tokens"`
	CachedReadTokens    int64     `json:"cached_read_tokens"`
	CacheWriteTokens    int64     `json:"cache_write_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	RepeatedInputTokens int64     `json:"repeated_input_tokens"`
	CostUSD             float64   `json:"cost_usd"`
	LatencyMS           int       `json:"latency_ms"`
	InferredTaskID      *string   `json:"inferred_task_id"`
	BreakpointsAdded    int       `json:"breakpoints_added"`
	GuardSignal         string    `json:"guard_signal"`
	GuardAction         string    `json:"guard_action"`
}

// TaskWithCalls returns a task and its calls in order.
func (s *Store) TaskWithCalls(ctx context.Context, id string) (Task, []TaskCall, error) {
	t, err := scanTask(s.pool.QueryRow(ctx, taskSelect+" WHERE t.id = $1", id))
	if err != nil {
		return Task{}, nil, mapErr(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, model, status,
		input_tokens + cached_read_tokens + cache_write_tokens + cache_write_1h_tokens,
		cached_read_tokens, cache_write_tokens + cache_write_1h_tokens, output_tokens, repeated_input_tokens,
		cost_usd::float8, latency_ms, inferred_task_id, breakpoints_added, guard_signal, guard_action
		FROM requests WHERE task_id = $1 ORDER BY ts`, id)
	if err != nil {
		return t, nil, err
	}
	calls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TaskCall, error) {
		var c TaskCall
		return c, r.Scan(&c.ID, &c.Time, &c.Model, &c.Status, &c.ContextTokens, &c.CachedReadTokens,
			&c.CacheWriteTokens, &c.OutputTokens, &c.RepeatedInputTokens, &c.CostUSD, &c.LatencyMS, &c.InferredTaskID,
			&c.BreakpointsAdded, &c.GuardSignal, &c.GuardAction)
	})
	return t, calls, err
}
