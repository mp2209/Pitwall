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

	"github.com/mp2209/pitwall/internal/api"
	"github.com/mp2209/pitwall/internal/config"
)

func main() {
	configPath := flag.String("config", envOr("PITWALL_CONFIG", "config/pitwall.yaml"), "path to the config file")
	healthcheck := flag.Bool("healthcheck", false, "check a running gateway's /healthz and exit (for container health checks)")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(envOr("PITWALL_HEALTH_URL", "http://127.0.0.1:8080/healthz")))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "path", *configPath, "err", err)
		os.Exit(1)
	}
	if len(cfg.DevKeys) == 0 {
		log.Warn("dev_keys is empty: the gateway accepts unauthenticated requests; use this only on your own machine")
	}
	for _, p := range cfg.Providers {
		if p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			log.Warn("provider key not set; calls to this provider will fail", "provider", p.Name, "env", p.APIKeyEnv)
		}
	}

	srv := api.New(cfg, api.Options{Log: log})
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: agent streams can run for many minutes.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("pitwall gateway listening", "addr", cfg.Listen, "providers", len(cfg.Providers), "model_rules", len(cfg.Models))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	log.Info("shutting down; waiting up to 30s for open streams")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Warn("shutdown", "err", err)
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
