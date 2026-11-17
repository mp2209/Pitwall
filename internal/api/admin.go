package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mp2209/pitwall/internal/auth"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/store"
)

// adminRoutes is the admin API. Callers authenticate with the bootstrap
// admin token or with a virtual key whose user has the admin role.
func (s *Server) adminRoutes(r chi.Router) {
	r.Use(s.adminAuth)
	r.Get("/teams", s.listTeams)
	r.Post("/teams", s.createTeam)
	r.Patch("/teams/{id}", s.updateTeam)
	r.Get("/users", s.listUsers)
	r.Post("/users", s.createUser)
	r.Get("/keys", s.listKeys)
	r.Post("/keys", s.createKey)
	r.Patch("/keys/{id}", s.updateKey)
	r.Delete("/keys/{id}", s.revokeKey)
	r.Get("/prices", s.listPrices)
	r.Post("/prices", s.addPrice)
	r.Get("/requests", s.listRequests)
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := clientKey(r)
		if s.adminToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.adminToken)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if s.keys != nil {
			k, err := s.keys.Authenticate(r.Context(), token)
			if err == nil && k.Role == auth.RoleAdmin {
				next.ServeHTTP(w, r.WithContext(withKey(r.Context(), k)))
				return
			}
			if err == nil {
				adminError(w, http.StatusForbidden, "this key's user is not an admin")
				return
			}
		}
		adminError(w, http.StatusUnauthorized, "admin token or admin key required")
	})
}

func adminError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}

// storeError maps store errors to statuses.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		adminError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		adminError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrBadRef):
		adminError(w, http.StatusBadRequest, err.Error())
	default:
		adminError(w, http.StatusInternalServerError, err.Error())
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		adminError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func validBudget(w http.ResponseWriter, usd *float64) bool {
	if usd != nil && *usd < 0 {
		adminError(w, http.StatusBadRequest, "monthly_budget_usd must be 0 (no limit) or more")
		return false
	}
	return true
}

func validModels(w http.ResponseWriter, patterns []string) bool {
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			adminError(w, http.StatusBadRequest, fmt.Sprintf("allowed_models: bad pattern %q", p))
			return false
		}
	}
	return true
}

// spend is a scope's month-to-date spend, live from Redis when budgets are
// on and from the request log otherwise.
type spend struct {
	SpentUSD    float64 `json:"spent_this_month_usd"`
	ReservedUSD float64 `json:"reserved_usd"`
}

func (s *Server) spendFor(ctx context.Context, scopeID string) spend {
	if s.budget != nil {
		if spent, held, err := s.budget.Status(ctx, scopeID); err == nil {
			return spend{SpentUSD: spent.USD(), ReservedUSD: held.USD()}
		}
	}
	spent, err := s.store.SpendSince(ctx, scopeID, metering.MonthStart(time.Now()))
	if err != nil {
		s.log.Warn("spend lookup failed", "scope", scopeID, "err", err)
	}
	return spend{SpentUSD: spent.USD()}
}

// --- teams and users ---------------------------------------------------------

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	type row struct {
		store.Team
		spend
	}
	out := make([]row, len(teams))
	for i, t := range teams {
		out[i] = row{t, s.spendFor(r.Context(), auth.TeamScope(t.ID))}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name             string  `json:"name"`
		MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
	}
	if !decodeJSON(w, r, &in) || !validBudget(w, &in.MonthlyBudgetUSD) {
		return
	}
	if strings.TrimSpace(in.Name) == "" {
		adminError(w, http.StatusBadRequest, "name is required")
		return
	}
	t, err := s.store.CreateTeam(r.Context(), strings.TrimSpace(in.Name), in.MonthlyBudgetUSD)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
	}
	if !decodeJSON(w, r, &in) || !validBudget(w, in.MonthlyBudgetUSD) {
		return
	}
	if in.MonthlyBudgetUSD == nil {
		adminError(w, http.StatusBadRequest, "monthly_budget_usd is required")
		return
	}
	t, err := s.store.UpdateTeamBudget(r.Context(), id, *in.MonthlyBudgetUSD)
	if err != nil {
		storeError(w, err)
		return
	}
	s.invalidateKeys()
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": users})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email  string `json:"email"`
		TeamID *int64 `json:"team_id"`
		Role   string `json:"role"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Role == "" {
		in.Role = auth.RoleDev
	}
	if in.Role != auth.RoleDev && in.Role != auth.RoleAdmin {
		adminError(w, http.StatusBadRequest, `role must be "dev" or "admin"`)
		return
	}
	if !strings.Contains(in.Email, "@") {
		adminError(w, http.StatusBadRequest, "email is required")
		return
	}
	u, err := s.store.CreateUser(r.Context(), strings.TrimSpace(in.Email), in.TeamID, in.Role)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// --- keys --------------------------------------------------------------------

type keyRow struct {
	store.KeyInfo
	spend
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListKeys(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	out := make([]keyRow, len(keys))
	for i, k := range keys {
		out[i] = keyRow{k, s.spendFor(r.Context(), auth.KeyScope(k.ID))}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var in store.NewKey
	if !decodeJSON(w, r, &in) || !validBudget(w, &in.MonthlyBudgetUSD) || !validModels(w, in.AllowedModels) {
		return
	}
	if in.UserID == 0 {
		adminError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	info, plain, err := s.store.CreateKey(r.Context(), in)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Key  string `json:"key"`
		Note string `json:"note"`
		store.KeyInfo
	}{plain, "Store this key now: Pitwall keeps only its hash and cannot show it again.", info})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in store.KeyUpdate
	if !decodeJSON(w, r, &in) || !validBudget(w, in.MonthlyBudgetUSD) {
		return
	}
	if in.AllowedModels != nil && !validModels(w, *in.AllowedModels) {
		return
	}
	info, err := s.store.UpdateKey(r.Context(), id, in)
	if err != nil {
		storeError(w, err)
		return
	}
	s.invalidateKeys()
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	info, err := s.store.RevokeKey(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	s.invalidateKeys()
	writeJSON(w, http.StatusOK, info)
}

// invalidateKeys makes key changes take effect at once on this replica;
// other replicas pick them up when their cache entries expire.
func (s *Server) invalidateKeys() {
	if s.keys != nil {
		s.keys.Invalidate()
	}
}

// --- prices and requests -----------------------------------------------------

func (s *Server) listPrices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.prices.All()})
}

// addPrice records a price change. Existing rows never change; a new row
// with a later effective_from applies to calls from that time on.
func (s *Server) addPrice(w http.ResponseWriter, r *http.Request) {
	var in metering.Price
	if !decodeJSON(w, r, &in) {
		return
	}
	switch {
	case in.Model == "":
		adminError(w, http.StatusBadRequest, "model is required")
		return
	case in.Speed != "" && in.Speed != "fast":
		adminError(w, http.StatusBadRequest, `speed must be "" or "fast"`)
		return
	case in.Input < 0 || in.Output < 0 || in.CachedRead < 0 || in.CacheWrite < 0 || in.CacheWrite1h < 0:
		adminError(w, http.StatusBadRequest, "rates cannot be negative")
		return
	}
	if in.EffectiveFrom.IsZero() {
		in.EffectiveFrom = time.Now().UTC()
	}
	if err := s.store.AddPrice(r.Context(), in); err != nil {
		storeError(w, err)
		return
	}
	if err := s.ReloadPrices(r.Context()); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

// ReloadPrices replaces the in-memory price book from the database.
func (s *Server) ReloadPrices(ctx context.Context) error {
	prices, err := s.store.ListPrices(ctx)
	if err != nil {
		return err
	}
	s.prices.Replace(prices)
	return nil
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	var keyID int64
	if v := r.URL.Query().Get("key_id"); v != "" {
		var err error
		if keyID, err = strconv.ParseInt(v, 10, 64); err != nil {
			adminError(w, http.StatusBadRequest, "key_id must be an integer")
			return
		}
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.store.RecentRequests(r.Context(), keyID, limit)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}
