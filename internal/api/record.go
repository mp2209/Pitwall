package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/store"
)

// Record describes one model call once it has finished.
type Record struct {
	ID            string
	Time          time.Time
	ClientFormat  string
	Model         string // as the client asked for it
	UpstreamModel string
	Provider      string
	Stream        bool
	Status        int
	Usage         metering.Usage
	Latency       time.Duration
	TTFT          time.Duration // streams only: time to the first event
	Overhead      time.Duration // latency not spent waiting on the provider
	ClientGone    bool          // the client disconnected before the end
	Error         string

	KeyID     int64 // 0 without virtual keys (dev mode)
	KeyPrefix string
	TeamID    int64
	Cost      metering.Nanos
	Priced    bool           // a price row (or a free provider) covered the call
	Reserved  metering.Nanos // worst case held against the budget
}

// Recorder receives every finished call. Implementations must not block:
// Record runs on the request path.
type Recorder interface {
	Record(ctx context.Context, r Record)
}

// Recorders fans a record out to several recorders.
type Recorders []Recorder

func (rs Recorders) Record(ctx context.Context, r Record) {
	for _, rec := range rs {
		rec.Record(ctx, r)
	}
}

// LogRecorder writes each call as a structured log line.
type LogRecorder struct{ Log *slog.Logger }

func (l LogRecorder) Record(ctx context.Context, r Record) {
	attrs := []slog.Attr{
		slog.String("id", r.ID),
		slog.String("format", r.ClientFormat),
		slog.String("model", r.Model),
		slog.String("upstream_model", r.UpstreamModel),
		slog.String("provider", r.Provider),
		slog.Bool("stream", r.Stream),
		slog.Int("status", r.Status),
		slog.Int64("input_tokens", r.Usage.InputTokens),
		slog.Int64("cached_read_tokens", r.Usage.CachedReadTokens),
		slog.Int64("cache_write_tokens", r.Usage.CacheWriteTokens),
		slog.Int64("cache_write_1h_tokens", r.Usage.CacheWrite1hTokens),
		slog.Int64("output_tokens", r.Usage.OutputTokens),
		slog.Bool("estimated", r.Usage.Estimated),
		slog.Float64("cost_usd", r.Cost.USD()),
		slog.Bool("priced", r.Priced),
		slog.Int64("latency_ms", r.Latency.Milliseconds()),
		slog.Float64("overhead_ms", float64(r.Overhead.Microseconds())/1000),
	}
	if r.KeyPrefix != "" {
		attrs = append(attrs, slog.String("key", r.KeyPrefix))
	}
	if r.Stream {
		attrs = append(attrs, slog.Int64("ttft_ms", r.TTFT.Milliseconds()))
	}
	if r.ClientGone {
		attrs = append(attrs, slog.Bool("client_gone", true))
	}
	level := slog.LevelInfo
	if r.Error != "" {
		attrs = append(attrs, slog.String("error", r.Error))
		level = slog.LevelWarn
	}
	l.Log.LogAttrs(ctx, level, "model call", attrs...)
}

// DBRecorder queues each call for the requests table. Calls without a
// virtual key (dev mode) have no key_id and are not stored.
type DBRecorder struct{ W *store.RequestWriter }

func (d DBRecorder) Record(_ context.Context, r Record) {
	if r.KeyID == 0 {
		return
	}
	row := store.RequestRow{
		ID: r.ID, Time: r.Time, KeyID: r.KeyID,
		ClientFormat: r.ClientFormat, Model: r.Model, UpstreamModel: r.UpstreamModel, Provider: r.Provider,
		Stream: r.Stream, Status: r.Status,
		InputTokens: r.Usage.InputTokens, CachedReadTokens: r.Usage.CachedReadTokens,
		CacheWriteTokens: r.Usage.CacheWriteTokens, CacheWrite1hTokens: r.Usage.CacheWrite1hTokens,
		OutputTokens: r.Usage.OutputTokens, Speed: r.Usage.Speed, InferenceGeo: r.Usage.InferenceGeo,
		Estimated: r.Usage.Estimated, Priced: r.Priced, Cost: r.Cost, Reserved: r.Reserved,
		LatencyMS: int(r.Latency.Milliseconds()), OverheadMS: float32(r.Overhead.Microseconds()) / 1000,
		ClientGone: r.ClientGone, Error: r.Error,
	}
	if r.TeamID != 0 {
		team := r.TeamID
		row.TeamID = &team
	}
	if r.Stream && r.TTFT > 0 {
		ms := int(r.TTFT.Milliseconds())
		row.TTFTMS = &ms
	}
	d.W.Enqueue(row)
}
