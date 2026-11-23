package store

import (
	"context"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mp2209/pitwall/internal/metering"
)

// RequestRow is one model call in the request log.
type RequestRow struct {
	ID                 string         `json:"id"`
	Time               time.Time      `json:"ts"`
	TaskID             *string        `json:"task_id"`
	KeyID              int64          `json:"key_id"`
	TeamID             *int64         `json:"team_id"`
	ClientFormat       string         `json:"client_format"`
	Model              string         `json:"model"`
	UpstreamModel      string         `json:"upstream_model"`
	Provider           string         `json:"provider"`
	Stream             bool           `json:"stream"`
	Status             int            `json:"status"`
	InputTokens        int64          `json:"input_tokens"`
	CachedReadTokens   int64          `json:"cached_read_tokens"`
	CacheWriteTokens   int64          `json:"cache_write_tokens"`
	CacheWrite1hTokens int64          `json:"cache_write_1h_tokens"`
	OutputTokens       int64          `json:"output_tokens"`
	Speed              string         `json:"speed"`
	InferenceGeo       string         `json:"inference_geo"`
	Estimated          bool           `json:"estimated"`
	Priced             bool           `json:"priced"`
	Cost               metering.Nanos `json:"-"`
	CostUSD            float64        `json:"cost_usd"`
	Reserved           metering.Nanos `json:"-"`
	LatencyMS          int            `json:"latency_ms"`
	TTFTMS             *int           `json:"ttft_ms"`
	OverheadMS         float32        `json:"overhead_ms"`
	ClientGone         bool           `json:"client_gone"`
	Error              string         `json:"error"`

	InferredTaskID      *string `json:"inferred_task_id"`
	RepeatedInputTokens int64   `json:"repeated_input_tokens"`
	BreakpointsAdded    int     `json:"breakpoints_added"`
	GuardSignal         string  `json:"guard_signal"`
	GuardAction         string  `json:"guard_action"`

	// Task attributes, used to create or update the task's row; not
	// columns of requests.
	TaskSource     string `json:"-"`
	TaskExternalID string `json:"-"`
	Agent          string `json:"-"`
	ConfigLabel    string `json:"-"`
}

// PromptTokens is all input of the call, cached or not: its context size.
func (r RequestRow) PromptTokens() int64 {
	return r.InputTokens + r.CachedReadTokens + r.CacheWriteTokens + r.CacheWrite1hTokens
}

var requestColumns = []string{
	"id", "ts", "task_id", "key_id", "team_id", "client_format", "model", "upstream_model", "provider",
	"stream", "status", "input_tokens", "cached_read_tokens", "cache_write_tokens", "cache_write_1h_tokens",
	"output_tokens", "speed", "inference_geo", "estimated", "priced", "cost_usd", "reserved_usd",
	"latency_ms", "ttft_ms", "overhead_ms", "client_gone", "error", "inferred_task_id", "repeated_input_tokens",
	"breakpoints_added", "guard_signal", "guard_action",
}

// usdNumeric stores nanodollars exactly as numeric(18,9).
func usdNumeric(n metering.Nanos) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(int64(n)), Exp: -9, Valid: true}
}

// InsertRequests writes rows with COPY, the fastest bulk path, and folds
// them into their tasks' totals in the same transaction, so a task row
// always agrees with its calls.
func (s *Store) InsertRequests(ctx context.Context, rows []RequestRow) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"requests"}, requestColumns,
			pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
				r := rows[i]
				return []any{
					r.ID, r.Time, r.TaskID, r.KeyID, r.TeamID, r.ClientFormat, r.Model, r.UpstreamModel, r.Provider,
					r.Stream, r.Status, r.InputTokens, r.CachedReadTokens, r.CacheWriteTokens, r.CacheWrite1hTokens,
					r.OutputTokens, r.Speed, r.InferenceGeo, r.Estimated, r.Priced, usdNumeric(r.Cost), usdNumeric(r.Reserved),
					r.LatencyMS, r.TTFTMS, r.OverheadMS, r.ClientGone, r.Error, r.InferredTaskID, r.RepeatedInputTokens,
					r.BreakpointsAdded, r.GuardSignal, r.GuardAction,
				}, nil
			}))
		if err != nil {
			return err
		}
		return upsertTasks(ctx, tx, rows)
	})
}

// RecentRequests lists the newest calls, optionally for one key.
func (s *Store) RecentRequests(ctx context.Context, keyID int64, limit int) ([]RequestRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, task_id, key_id, team_id, client_format, model, upstream_model,
		provider, stream, status, input_tokens, cached_read_tokens, cache_write_tokens, cache_write_1h_tokens,
		output_tokens, speed, inference_geo, estimated, priced, (cost_usd * 1000000000)::bigint,
		(reserved_usd * 1000000000)::bigint, latency_ms, ttft_ms, overhead_ms, client_gone, error,
		inferred_task_id, repeated_input_tokens, breakpoints_added, guard_signal, guard_action
		FROM requests WHERE ($1 = 0 OR key_id = $1) ORDER BY ts DESC LIMIT $2`, keyID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RequestRow, error) {
		var r RequestRow
		err := row.Scan(&r.ID, &r.Time, &r.TaskID, &r.KeyID, &r.TeamID, &r.ClientFormat, &r.Model, &r.UpstreamModel,
			&r.Provider, &r.Stream, &r.Status, &r.InputTokens, &r.CachedReadTokens, &r.CacheWriteTokens,
			&r.CacheWrite1hTokens, &r.OutputTokens, &r.Speed, &r.InferenceGeo, &r.Estimated, &r.Priced,
			&r.Cost, &r.Reserved, &r.LatencyMS, &r.TTFTMS, &r.OverheadMS, &r.ClientGone, &r.Error,
			&r.InferredTaskID, &r.RepeatedInputTokens, &r.BreakpointsAdded, &r.GuardSignal, &r.GuardAction)
		r.CostUSD = r.Cost.USD()
		return r, err
	})
}

// RequestWriter batches request rows into Postgres off the request path.
// Enqueue never blocks: if the buffer is full (the database is down or too
// slow), the row is dropped and counted rather than delaying an agent.
type RequestWriter struct {
	store     *Store
	log       *slog.Logger
	ch        chan RequestRow
	batchSize int
	every     time.Duration
	done      chan struct{}

	mu     sync.RWMutex
	closed bool

	dropped atomic.Int64
	written atomic.Int64
}

// WriterOptions tunes a RequestWriter; zero values get defaults.
type WriterOptions struct {
	Buffer     int           // queued rows before dropping, default 10000
	BatchSize  int           // rows per COPY, default 500
	FlushEvery time.Duration // longest a row waits, default 250ms
}

// NewRequestWriter starts the background writer.
func NewRequestWriter(s *Store, log *slog.Logger, o WriterOptions) *RequestWriter {
	if o.Buffer == 0 {
		o.Buffer = 10_000
	}
	if o.BatchSize == 0 {
		o.BatchSize = 500
	}
	if o.FlushEvery == 0 {
		o.FlushEvery = 250 * time.Millisecond
	}
	w := &RequestWriter{store: s, log: log, ch: make(chan RequestRow, o.Buffer),
		batchSize: o.BatchSize, every: o.FlushEvery, done: make(chan struct{})}
	go w.run()
	return w
}

// Enqueue queues a row, returning false if it was dropped.
func (w *RequestWriter) Enqueue(r RequestRow) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return false
	}
	select {
	case w.ch <- r:
		return true
	default:
		if n := w.dropped.Add(1); n == 1 || n%1000 == 0 {
			w.log.Warn("request log buffer full; dropping rows", "dropped_total", n)
		}
		return false
	}
}

// Stats reports rows written and dropped since start.
func (w *RequestWriter) Stats() (written, dropped int64) { return w.written.Load(), w.dropped.Load() }

func (w *RequestWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	batch := make([]RequestRow, 0, w.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := w.store.InsertRequests(ctx, batch)
		cancel()
		if err != nil {
			w.dropped.Add(int64(len(batch)))
			w.log.Error("writing request log failed; rows dropped", "rows", len(batch), "err", err)
		} else {
			w.written.Add(int64(len(batch)))
		}
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-w.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close stops accepting rows and waits for queued ones to be written.
func (w *RequestWriter) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
