// Package api serves the gateway's two client-facing formats,
// POST /v1/chat/completions (OpenAI) and POST /v1/messages (Anthropic), and
// the admin API under /admin.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/providers"
	"github.com/mp2209/pitwall/internal/store"
)

// Server is the gateway's HTTP handler set.
type Server struct {
	cfg        *config.Config
	router     *providers.Router
	client     *http.Client
	recorder   Recorder
	log        *slog.Logger
	keys       *auth.Authenticator
	store      *store.Store
	prices     *metering.PriceBook
	budget     *metering.Budget
	adminToken string

	unpriced sync.Map // model names already warned about
}

// Options configures a Server. Everything is optional: with no Keys the
// gateway runs in dev mode (Config.DevKeys or no auth), with no Budget calls
// are not budgeted, and with no Store there is no admin API.
type Options struct {
	Client   *http.Client
	Recorder Recorder
	Log      *slog.Logger
	Keys     *auth.Authenticator
	Store    *store.Store
	Prices   *metering.PriceBook
	Budget   *metering.Budget
	// AdminToken, if set, is accepted as a bearer token on /admin besides
	// keys whose user has the admin role. It bootstraps the first admin.
	AdminToken string
}

func New(cfg *config.Config, opts Options) *Server {
	s := &Server{
		cfg: cfg, router: providers.NewRouter(cfg), client: opts.Client, recorder: opts.Recorder, log: opts.Log,
		keys: opts.Keys, store: opts.Store, prices: opts.Prices, budget: opts.Budget, adminToken: opts.AdminToken,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.client == nil {
		s.client = providers.NewHTTPClient()
	}
	if s.recorder == nil {
		s.recorder = LogRecorder{Log: s.log}
	}
	if s.prices == nil {
		s.prices = metering.NewPriceBook(nil)
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
	if s.store != nil {
		r.Route("/admin", s.adminRoutes)
	}
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

type ctxKey struct{}

func withKey(ctx context.Context, k *auth.Key) context.Context {
	return context.WithValue(ctx, ctxKey{}, k)
}

// keyFrom returns the caller's virtual key, or nil in dev mode.
func keyFrom(ctx context.Context) *auth.Key {
	k, _ := ctx.Value(ctxKey{}).(*auth.Key)
	return k
}

// authenticate checks virtual keys when a key store is configured, and
// otherwise falls back to the dev_keys list (or no check if it is empty).
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.keys != nil {
			k, err := s.keys.Authenticate(r.Context(), clientKey(r))
			switch {
			case errors.Is(err, auth.ErrInvalidKey), errors.Is(err, auth.ErrRevokedKey):
				writeError(w, clientFormat(r), http.StatusUnauthorized, err.Error())
				return
			case err != nil:
				s.log.Error("key lookup failed", "err", err)
				writeError(w, clientFormat(r), http.StatusServiceUnavailable, "Pitwall could not check the API key; try again shortly")
				return
			}
			next.ServeHTTP(w, r.WithContext(withKey(r.Context(), k)))
			return
		}
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
		writeError(w, clientFormat(r), http.StatusUnauthorized, auth.ErrInvalidKey.Error())
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var models []providers.ListedModel
	key := keyFrom(r.Context())
	for _, m := range s.router.Models() {
		if key == nil || key.AllowsModel(m.ID) {
			models = append(models, m)
		}
	}
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
	prompt    []byte // the client's request body
	maxOutput int64  // output token limit, for the budget's worst case
	speed     string // "fast" if the client asked for fast mode
	key       *auth.Key

	reservation *metering.Reservation
}

func (c *call) record() Record {
	r := Record{
		ID:            c.id,
		Time:          c.start,
		ClientFormat:  c.format,
		Model:         c.model,
		UpstreamModel: c.route.Model,
		Provider:      c.route.Upstream.Name,
		Stream:        c.stream,
	}
	if c.key != nil {
		r.KeyID, r.KeyPrefix, r.TeamID = c.key.ID, c.key.Prefix, c.key.TeamID
	}
	return r
}
