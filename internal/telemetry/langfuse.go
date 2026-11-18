package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mp2209/pitwall/internal/api"
)

// LangfuseConfig points the exporter at a Langfuse project.
type LangfuseConfig struct {
	Host      string // for example http://langfuse-web:3000
	PublicKey string
	SecretKey string
	// FlushEvery and BatchSize bound how long events wait and how many go
	// in one request. MaxField caps each input or output, in bytes.
	FlushEvery time.Duration
	BatchSize  int
	MaxField   int
}

// Langfuse sends one trace per task and one generation per model call to
// Langfuse's ingestion API, off the request path and after redaction. Each
// generation's input is only what the call added to its task's conversation,
// so a trace reads as the run's turns instead of the full prompt resent N
// times.
type Langfuse struct {
	cfg    LangfuseConfig
	client *http.Client
	log    *slog.Logger
	ch     chan ingestionEvent
	done   chan struct{}

	mu     sync.RWMutex
	closed bool

	sent, dropped, failed atomic.Int64
}

type ingestionEvent struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Body      any    `json:"body"`
}

// NewLangfuse starts the exporter.
func NewLangfuse(cfg LangfuseConfig, log *slog.Logger) *Langfuse {
	if cfg.FlushEvery == 0 {
		cfg.FlushEvery = time.Second
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 50
	}
	if cfg.MaxField == 0 {
		cfg.MaxField = 32 << 10
	}
	cfg.Host = strings.TrimRight(cfg.Host, "/")
	l := &Langfuse{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}, log: log,
		ch: make(chan ingestionEvent, 5000), done: make(chan struct{})}
	go l.run()
	return l
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func eventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// field redacts and caps a value for Langfuse. JSON that is still valid
// after redaction is sent as JSON; anything cut short is sent as text.
func (l *Langfuse) field(raw string) any {
	red := Redact(raw)
	if len(red) > l.cfg.MaxField {
		return red[:l.cfg.MaxField] + fmt.Sprintf("…[%d more bytes not sent]", len(red)-l.cfg.MaxField)
	}
	if json.Valid([]byte(red)) {
		return json.RawMessage(red)
	}
	return red
}

// Record implements api.Recorder.
func (l *Langfuse) Record(_ context.Context, r api.Record) {
	traceID := r.TaskID
	if traceID == "" {
		traceID = r.ID // untracked call: a trace of its own
	}
	// Every call sends the task's trace with the same fields, start time
	// included. Langfuse treats trace-create as an upsert, so the trace
	// exists whichever call (or gateway replica) Langfuse sees first.
	start := r.TaskStart
	if start.IsZero() || r.TaskID == "" {
		start = r.Time
	}
	name := r.TaskExternalID
	if name == "" {
		name = "task " + traceID
	}
	var tags []string
	for _, t := range []string{r.Agent, r.ConfigLabel} {
		if t != "" {
			tags = append(tags, t)
		}
	}
	events := []ingestionEvent{{ID: eventID(), Timestamp: ts(r.Time), Type: "trace-create", Body: map[string]any{
		"id": traceID, "timestamp": ts(start), "name": name, "userId": r.KeyPrefix, "tags": tags,
		"metadata": map[string]any{"agent": r.Agent, "config_label": r.ConfigLabel, "source": r.TaskSource},
	}}}

	var input []any
	for _, m := range r.NewMessages {
		input = append(input, map[string]any{"role": m.Role, "content": l.field(string(m.Content))})
	}
	gen := map[string]any{
		"id": r.ID, "traceId": traceID, "name": fmt.Sprintf("call %d", max(r.CallIndex, 1)),
		"startTime": ts(r.Time), "endTime": ts(r.Time.Add(r.Latency)),
		"model": r.UpstreamModel, "input": input, "output": l.field(r.OutputText),
		"usageDetails": map[string]int64{
			"input": r.Usage.InputTokens, "cache_read_input_tokens": r.Usage.CachedReadTokens,
			"cache_creation_input_tokens": r.Usage.CacheWrites(), "output": r.Usage.OutputTokens,
		},
		"metadata": map[string]any{
			"request_id": r.ID, "provider": r.Provider, "requested_model": r.Model, "client_format": r.ClientFormat,
			"status": r.Status, "estimated": r.Usage.Estimated, "repeated_input_tokens": r.RepeatedInputTokens,
			"inferred_task_id": r.InferredTaskID,
		},
		"level": "DEFAULT",
	}
	if r.Priced {
		gen["costDetails"] = map[string]float64{"total": r.Cost.USD()}
	}
	if r.Stream && r.TTFT > 0 {
		gen["completionStartTime"] = ts(r.Time.Add(r.TTFT))
	}
	if r.Error != "" {
		gen["level"], gen["statusMessage"] = "ERROR", Redact(r.Error)
	}
	events = append(events, ingestionEvent{ID: eventID(), Timestamp: ts(r.Time), Type: "generation-create", Body: gen})

	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, e := range events {
		if l.closed {
			l.dropped.Add(1)
			continue
		}
		select {
		case l.ch <- e:
		default:
			if n := l.dropped.Add(1); n == 1 || n%1000 == 0 {
				l.log.Warn("langfuse queue full; dropping trace events", "dropped_total", n)
			}
		}
	}
}

func (l *Langfuse) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.cfg.FlushEvery)
	defer ticker.Stop()
	var batch []ingestionEvent
	flush := func() {
		if len(batch) > 0 {
			l.send(batch)
			batch = nil
		}
	}
	for {
		select {
		case e, ok := <-l.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= l.cfg.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (l *Langfuse) send(batch []ingestionEvent) {
	body, err := json.Marshal(map[string]any{"batch": batch})
	if err != nil {
		l.failed.Add(int64(len(batch)))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.Host+"/api/public/ingestion", bytes.NewReader(body))
	if err != nil {
		l.failed.Add(int64(len(batch)))
		return
	}
	req.SetBasicAuth(l.cfg.PublicKey, l.cfg.SecretKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.client.Do(req)
	if err != nil {
		l.failed.Add(int64(len(batch)))
		l.log.Warn("langfuse unreachable; trace events dropped", "events", len(batch), "err", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusMultiStatus {
		l.failed.Add(int64(len(batch)))
		l.log.Warn("langfuse rejected trace events", "status", resp.StatusCode, "events", len(batch), "body", string(respBody))
		return
	}
	// 207 Multi-Status lists per-event errors.
	var result struct {
		Errors []struct {
			ID      string `json:"id"`
			Status  int    `json:"status"`
			Message string `json:"message"`
			Error   any    `json:"error"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(respBody, &result)
	if len(result.Errors) > 0 {
		l.failed.Add(int64(len(result.Errors)))
		first := result.Errors[0]
		l.log.Warn("langfuse rejected some trace events", "rejected", len(result.Errors), "of", len(batch),
			"first_status", first.Status, "first_message", first.Message, "first_error", fmt.Sprint(first.Error))
	}
	l.sent.Add(int64(len(batch) - len(result.Errors)))
}

// Stats reports events accepted by Langfuse, dropped from a full queue, and
// rejected or failed to send.
func (l *Langfuse) Stats() (sent, dropped, failed int64) {
	return l.sent.Load(), l.dropped.Load(), l.failed.Load()
}

// Close stops accepting events and waits for queued ones to be sent.
func (l *Langfuse) Close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.ch)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
