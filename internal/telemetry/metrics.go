// Package telemetry exports what the gateway records: Prometheus metrics,
// Langfuse traces (one trace per task, one generation per call), and the
// PII redaction applied before anything leaves for a trace store.
package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mp2209/pitwall/internal/api"
)

// Metrics is the gateway's Prometheus registry. Labels are kept bounded:
// a model label is only ever a priced model name (a client can't mint new
// series by asking for made-up models), and per-task numbers never become
// labels, since every task would create a new time series. They live in
// Postgres instead, which Grafana queries directly.
type Metrics struct {
	reg *prometheus.Registry

	calls          *prometheus.CounterVec
	tokens         *prometheus.CounterVec
	cost           *prometheus.CounterVec
	costUncached   *prometheus.CounterVec
	repeated       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	ttft           *prometheus.HistogramVec
	overhead       *prometheus.HistogramVec
	tasksStarted   *prometheus.CounterVec
	budgetRefusals prometheus.Counter
	clientGone     prometheus.Counter
	// GuardTriggers counts runaway-agent guard warnings and stops by signal.
	GuardTriggers *prometheus.CounterVec
	cacheOpt      *prometheus.CounterVec
	breakpoints   prometheus.Counter
}

var latencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60, 120, 300}

// NewMetrics registers the gateway's metrics.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(c prometheus.Collector) { reg.MustRegister(c) }
	m := &Metrics{reg: reg}
	m.calls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_calls_total", Help: "Model calls by provider, model, client format and HTTP status.",
	}, []string{"provider", "model", "format", "status"})
	m.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_tokens_total",
		Help: "Tokens by kind: input (uncached), cached_read, cache_write, cache_write_1h, output.",
	}, []string{"provider", "model", "kind"})
	m.cost = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_cost_usd_total", Help: "Cost of model calls in USD.",
	}, []string{"provider", "model"})
	// Savings can be negative (cache writes cost more than plain input), and
	// a counter can't go down, so this exports the cost the same calls would
	// have had without caching. Savings = this minus pitwall_cost_usd_total.
	m.costUncached = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_cost_without_cache_usd_total",
		Help: "What the calls would have cost with no prompt caching. Subtract pitwall_cost_usd_total for the savings.",
	}, []string{"provider", "model"})
	m.repeated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_repeated_input_tokens_total", Help: "Input tokens resent unchanged from the previous call of the same task.",
	}, []string{"provider", "model"})
	m.duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "pitwall_call_duration_seconds", Help: "Total time of a model call, gateway included.", Buckets: latencyBuckets,
	}, []string{"provider", "model", "stream"})
	m.ttft = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "pitwall_ttft_seconds", Help: "Time to the first streamed event.", Buckets: latencyBuckets,
	}, []string{"provider", "model"})
	m.overhead = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pitwall_gateway_overhead_seconds",
		Help:    "Latency the gateway adds, outside the provider's own time.",
		Buckets: []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25},
	}, []string{"format", "stream"})
	m.tasksStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_tasks_started_total", Help: "Agent tasks started, by how they were identified.",
	}, []string{"source"})
	m.budgetRefusals = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pitwall_budget_refusals_total", Help: "Calls refused because a budget was exhausted.",
	})
	m.clientGone = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pitwall_client_disconnects_total", Help: "Calls the client abandoned before the end.",
	})
	m.GuardTriggers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_guard_triggers_total", Help: "Runaway-agent guard warnings and stops, by signal and action.",
	}, []string{"signal", "action"})
	m.cacheOpt = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pitwall_cache_optimizer_calls_total",
		Help: "Anthropic-format calls by what the cache optimizer did: applied, client_managed, too_short, not_applicable, off.",
	}, []string{"outcome"})
	m.breakpoints = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pitwall_cache_breakpoints_added_total", Help: "cache_control breakpoints the gateway added.",
	})
	for _, c := range []prometheus.Collector{m.calls, m.tokens, m.cost, m.costUncached, m.repeated, m.duration,
		m.ttft, m.overhead, m.tasksStarted, m.budgetRefusals, m.clientGone, m.GuardTriggers, m.cacheOpt, m.breakpoints} {
		f(c)
	}
	return m
}

// Handler serves the metrics in Prometheus' text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

// AddGauge registers a value read at scrape time, such as the request log's
// dropped-row count.
func (m *Metrics) AddGauge(name, help string, read func() float64) {
	m.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, read))
}

// modelLabel keeps the model label bounded.
func modelLabel(r api.Record) string {
	if r.Priced && r.Status < 400 && r.UpstreamModel != "" {
		return r.UpstreamModel
	}
	return "other"
}

// Record implements api.Recorder.
func (m *Metrics) Record(_ context.Context, r api.Record) {
	model := modelLabel(r)
	m.calls.WithLabelValues(r.Provider, model, r.ClientFormat, strconv.Itoa(r.Status)).Inc()
	if r.Status == http.StatusPaymentRequired {
		m.budgetRefusals.Inc()
	}
	if r.ClientGone {
		m.clientGone.Inc()
	}
	if r.TaskID != "" && r.CallIndex == 1 {
		m.tasksStarted.WithLabelValues(r.TaskSource).Inc()
	}
	for _, e := range r.GuardEvents {
		signal, action, _ := strings.Cut(e, ":")
		m.GuardTriggers.WithLabelValues(signal, action).Inc()
	}
	if r.CacheOptimizer != "" {
		m.cacheOpt.WithLabelValues(r.CacheOptimizer).Inc()
	}
	if r.BreakpointsAdded > 0 {
		m.breakpoints.Add(float64(r.BreakpointsAdded))
	}
	if r.Status == 0 || r.Status >= 400 && !r.ClientGone {
		return
	}
	u := r.Usage
	for kind, n := range map[string]int64{
		"input": u.InputTokens, "cached_read": u.CachedReadTokens, "cache_write": u.CacheWriteTokens,
		"cache_write_1h": u.CacheWrite1hTokens, "output": u.OutputTokens,
	} {
		if n > 0 {
			m.tokens.WithLabelValues(r.Provider, model, kind).Add(float64(n))
		}
	}
	m.cost.WithLabelValues(r.Provider, model).Add(r.Cost.USD())
	m.costUncached.WithLabelValues(r.Provider, model).Add(max(r.Cost+r.CacheSavings, 0).USD())
	if r.RepeatedInputTokens > 0 {
		m.repeated.WithLabelValues(r.Provider, model).Add(float64(r.RepeatedInputTokens))
	}
	stream := strconv.FormatBool(r.Stream)
	m.duration.WithLabelValues(r.Provider, model, stream).Observe(r.Latency.Seconds())
	m.overhead.WithLabelValues(r.ClientFormat, stream).Observe(r.Overhead.Seconds())
	if r.Stream && r.TTFT > 0 {
		m.ttft.WithLabelValues(r.Provider, model).Observe(r.TTFT.Seconds())
	}
}
