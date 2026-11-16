// Command mockprovider runs a fake OpenAI- and Anthropic-format model
// provider for tests, load tests and local development.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/mp2209/pitwall/mockprovider"
)

func main() {
	addr := flag.String("addr", envOr("MOCK_ADDR", ":9000"), "listen address")
	delay := flag.Duration("delay", envDuration("MOCK_DELAY_MS"), "delay before the first byte")
	chunkDelay := flag.Duration("chunk-delay", envDuration("MOCK_CHUNK_DELAY_MS"), "delay between stream chunks")
	errorRate := flag.Float64("error-rate", envFloat("MOCK_ERROR_RATE"), "share of requests that fail with 500")
	flag.Parse()

	srv := mockprovider.New(mockprovider.Config{Delay: *delay, ChunkDelay: *chunkDelay, ErrorRate: *errorRate})
	slog.Info("mock provider listening", "addr", *addr, "delay", *delay, "chunk_delay", *chunkDelay, "error_rate", *errorRate)
	server := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		slog.Error("mock provider stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string) time.Duration {
	ms, _ := strconv.Atoi(os.Getenv(key))
	return time.Duration(ms) * time.Millisecond
}

func envFloat(key string) float64 {
	f, _ := strconv.ParseFloat(os.Getenv(key), 64)
	return f
}
