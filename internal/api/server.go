// Package api serves the gateway's two client-facing formats:
// POST /v1/chat/completions (OpenAI) and POST /v1/messages (Anthropic).
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/providers"
)

// Server is the gateway's HTTP handler set.
type Server struct {
	cfg      *config.Config
	router   *providers.Router
	client   *http.Client
	recorder Recorder
	log      *slog.Logger
}

// Options configures a Server. Zero values get defaults.
type Options struct {
	Client   *http.Client
	Recorder Recorder
	Log      *slog.Logger
}

func New(cfg *config.Config, opts Options) *Server {
	s := &Server{cfg: cfg, router: providers.NewRouter(cfg), client: opts.Client, recorder: opts.Recorder, log: opts.Log}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.client == nil {
		s.client = providers.NewHTTPClient()
	}
	if s.recorder == nil {
		s.recorder = LogRecorder{Log: s.log}
	}
	return s
}

// Handler returns the gateway's routes.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		r.Get("/v1/models", s.handleModels)
		r.Post("/v1/chat/completions", s.handleChatCompletions)
		r.Post("/v1/messages", s.handleMessages)
		r.Post("/v1/messages/count_tokens", s.handleCountTokens)
	})
	return r
}

// clientFormat guesses which format a caller speaks from the path and
// headers, so even auth and routing errors come back in a shape its SDK
// can parse.
func clientFormat(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/messages") || r.Header.Get("anthropic-version") != "" {
		return config.KindAnthropic
	}
	return config.KindOpenAI
}

// clientKey reads the caller's key from either format's auth header.
func clientKey(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// authenticate is the Phase 1 stand-in for virtual keys: a fixed list of
// dev keys from the config, or no check at all when the list is empty.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.DevKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		key := clientKey(r)
		for _, k := range s.cfg.DevKeys {
			if subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, clientFormat(r), http.StatusUnauthorized, "invalid or missing Pitwall API key")
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.router.Models()
	if clientFormat(r) == config.KindAnthropic {
		type model struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		}
		data := make([]model, 0, len(models))
		for _, m := range models {
			data = append(data, model{ID: m.ID, Type: "model", DisplayName: m.ID, CreatedAt: "1970-01-01T00:00:00Z"})
		}
		resp := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
		if len(data) > 0 {
			resp["first_id"], resp["last_id"] = data[0].ID, data[len(data)-1].ID
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(models))
	for _, m := range models {
		data = append(data, model{ID: m.ID, Object: "model", OwnedBy: m.Provider})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// readBody reads a request body up to the configured limit.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request, format string) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, format, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, format, http.StatusBadRequest, "could not read request body")
		}
		return nil, false
	}
	return body, true
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "pw_" + hex.EncodeToString(b[:])
}

// call holds what the handlers know about one model call before it is sent.
type call struct {
	id        string
	start     time.Time
	format    string // what the client speaks
	model     string // what the client asked for
	route     providers.Route
	stream    bool
	promptLen int
}

func (c *call) record() Record {
	return Record{
		ID:            c.id,
		Time:          c.start,
		ClientFormat:  c.format,
		Model:         c.model,
		UpstreamModel: c.route.Model,
		Provider:      c.route.Upstream.Name,
		Stream:        c.stream,
	}
}
