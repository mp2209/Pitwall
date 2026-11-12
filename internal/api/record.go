package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
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
}

// Recorder receives every finished call. Implementations must not block:
// Record runs on the request path. Phase 2 adds a Postgres writer behind a
// queue.
type Recorder interface {
	Record(ctx context.Context, r Record)
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
		slog.Int64("output_tokens", r.Usage.OutputTokens),
		slog.Bool("estimated", r.Usage.Estimated),
		slog.Int64("latency_ms", r.Latency.Milliseconds()),
		slog.Float64("overhead_ms", float64(r.Overhead.Microseconds())/1000),
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
