// Command gateway runs the Pitwall gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mp2209/pitwall/internal/api"
	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/store"
	"github.com/mp2209/pitwall/internal/tasks"
	"github.com/mp2209/pitwall/internal/telemetry"
)

func main() { os.Exit(run()) }

func run() int {
	configPath := flag.String("config", envOr("PITWALL_CONFIG", "config/pitwall.yaml"), "path to the config file")
	healthcheck := flag.Bool("healthcheck", false, "check a running gateway's /healthz and exit (for container health checks)")
	flag.Parse()

	if *healthcheck {
		return runHealthcheck(envOr("PITWALL_HEALTH_URL", "http://127.0.0.1:8080/healthz"))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "path", *configPath, "err", err)
		return 1
	}
	for _, p := range cfg.Providers {
		if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			log.Warn("provider key not set; calls to this provider will fail", "provider", p.Name, "env", p.APIKeyEnv)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	metering.WarmTokenizer()

	metrics := telemetry.NewMetrics()
	opts, closeDeps, err := setup(ctx, cfg, log, metrics)
	if err != nil {
		log.Error("startup", "err", err)
		closeDeps(context.Background())
		return 1
	}
	recorders := api.Recorders{opts.Recorder, metrics}
	langfuse := langfuseFromEnv(log)
	if langfuse != nil {
		recorders = append(recorders, langfuse)
		metrics.AddGauge("pitwall_langfuse_events_dropped", "Trace events dropped or rejected by Langfuse.", func() float64 {
			_, dropped, failed := langfuse.Stats()
			return float64(dropped + failed)
		})
	}
	opts.Recorder = recorders
	srv := api.New(cfg, opts)
	if opts.Store != nil {
		go refreshPrices(ctx, srv, log)
	}
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: agent streams can run for many minutes.
	}

	var metricsServer *http.Server
	if addr := *cfg.MetricsListen; addr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", metrics.Handler())
		metricsServer = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server stopped", "err", err)
			}
		}()
		log.Info("prometheus metrics", "addr", addr, "path", "/metrics")
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("pitwall gateway listening", "addr", cfg.Listen, "providers", len(cfg.Providers), "model_rules", len(cfg.Models),
			"virtual_keys", opts.Keys != nil, "budgets", opts.Budget != nil)
		serveErr <- server.ListenAndServe()
	}()
	code := 0
	select {
	case <-ctx.Done():
		log.Info("shutting down; waiting up to 30s for open streams")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			code = 1
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	closeDeps(shutdownCtx)
	if langfuse != nil {
		if err := langfuse.Close(shutdownCtx); err != nil {
			log.Warn("langfuse queue not fully sent at shutdown", "err", err)
		}
	}
	if metricsServer != nil {
		_ = metricsServer.Shutdown(shutdownCtx)
	}
	return code
}

// langfuseFromEnv starts the Langfuse exporter when LANGFUSE_HOST and its
// keys are set.
func langfuseFromEnv(log *slog.Logger) *telemetry.Langfuse {
	host, pub, sec := os.Getenv("LANGFUSE_HOST"), os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY")
	if host == "" || pub == "" || sec == "" {
		log.Info("langfuse tracing off (set LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY to enable)")
		return nil
	}
	log.Info("langfuse tracing on: one trace per task, inputs and outputs redacted", "host", host)
	return telemetry.NewLangfuse(telemetry.LangfuseConfig{Host: host, PublicKey: pub, SecretKey: sec}, log)
}

// setup connects the optional backing services. With PITWALL_DATABASE_URL
// the gateway uses virtual keys, records every call in Postgres and serves
// the admin API; with PITWALL_REDIS_URL as well it enforces budgets. Without
// them it runs in dev mode.
func setup(ctx context.Context, cfg *config.Config, log *slog.Logger, metrics *telemetry.Metrics) (api.Options, func(context.Context), error) {
	opts := api.Options{Log: log, AdminToken: os.Getenv("PITWALL_ADMIN_TOKEN")}
	opts.Tracker = tasks.NewTracker(tasks.NewMemStore())
	opts.Tracker.IdleGap = cfg.TaskIdleGap
	var closers []func(context.Context)
	closeAll := func(ctx context.Context) {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i](ctx)
		}
	}

	filePrices, err := config.LoadPrices(cfg.PricesPath())
	if err != nil {
		return opts, closeAll, fmt.Errorf("load prices %s: %w", cfg.PricesPath(), err)
	}
	opts.Prices = metering.NewPriceBook(filePrices)
	logRec := api.LogRecorder{Log: log}
	opts.Recorder = logRec

	dbURL := os.Getenv("PITWALL_DATABASE_URL")
	if dbURL == "" {
		if len(cfg.DevKeys) == 0 {
			log.Warn("dev mode with no dev_keys: the gateway accepts unauthenticated requests; use this only on your own machine")
		} else {
			log.Info("dev mode: no database, checking dev_keys only; calls are logged but not stored")
		}
		return opts, closeAll, nil
	}

	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return opts, closeAll, err
	}
	closers = append(closers, func(context.Context) { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		return opts, closeAll, err
	}
	if n, err := st.SyncPrices(ctx, filePrices); err != nil {
		return opts, closeAll, err
	} else if n > 0 {
		log.Info("added price rows from the price file", "rows", n)
	}
	dbPrices, err := st.ListPrices(ctx)
	if err != nil {
		return opts, closeAll, err
	}
	opts.Prices.Replace(dbPrices)

	writer := store.NewRequestWriter(st, log, store.WriterOptions{})
	metrics.AddGauge("pitwall_request_log_dropped_rows", "Request log rows dropped because the database was down or slow.", func() float64 {
		_, dropped := writer.Stats()
		return float64(dropped)
	})
	closers = append(closers, func(ctx context.Context) {
		if err := writer.Close(ctx); err != nil {
			log.Warn("request log not fully flushed at shutdown", "err", err)
		}
		written, dropped := writer.Stats()
		log.Info("request log closed", "written", written, "dropped", dropped)
	})
	opts.Store = st
	opts.Keys = auth.NewAuthenticator(st, 30*time.Second)
	opts.Recorder = api.Recorders{logRec, api.DBRecorder{W: writer}}
	if len(cfg.DevKeys) > 0 {
		log.Warn("dev_keys is ignored when a database is configured; use virtual keys from the admin API")
	}
	if opts.AdminToken == "" {
		log.Warn("PITWALL_ADMIN_TOKEN is not set: only keys of admin users can use /admin")
	}

	redisURL := os.Getenv("PITWALL_REDIS_URL")
	if redisURL == "" {
		log.Warn("PITWALL_REDIS_URL is not set: budgets are not enforced")
		return opts, closeAll, nil
	}
	ropts, err := redis.ParseURL(redisURL)
	if err != nil {
		return opts, closeAll, fmt.Errorf("PITWALL_REDIS_URL: %w", err)
	}
	rdb := redis.NewClient(ropts)
	closers = append(closers, func(context.Context) { _ = rdb.Close() })
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		if !cfg.FailOpen() {
			return opts, closeAll, fmt.Errorf("redis: %w", err)
		}
		log.Error("redis unreachable at startup; budgets fail open until it is back", "err", err)
	}
	opts.Budget = metering.NewBudget(rdb, st.SpendSince)
	// Shared task state, so every replica infers the same tasks.
	opts.Tracker = tasks.NewTracker(tasks.NewRedisStore(rdb))
	opts.Tracker.IdleGap = cfg.TaskIdleGap
	return opts, closeAll, nil
}

// refreshPrices reloads the price table every minute, so a price added on
// another replica (or directly in the database) takes effect everywhere.
func refreshPrices(ctx context.Context, srv *api.Server, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := srv.ReloadPrices(ctx); err != nil && ctx.Err() == nil {
				log.Warn("price reload failed; keeping the previous table", "err", err)
			}
		}
	}
}

func runHealthcheck(url string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(os.Getenv("PITWALL_LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return l
}
