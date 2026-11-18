package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/tasks"
)

// budgetTimeout bounds each Redis round trip on the request path.
const budgetTimeout = 2 * time.Second

// reserveTokens estimates prompt tokens for a budget reservation. It leans
// high (three bytes per token rather than four) because the reservation is
// meant to be a worst case; the real count replaces it at settlement.
func reserveTokens(bodyBytes int) int64 { return int64((bodyBytes + 2) / 3) }

// reserve holds the call's worst-case cost against the key's and team's
// budgets. It writes an error and returns false if the call must not go on.
func (s *Server) reserve(w http.ResponseWriter, ctx context.Context, ex *exchange, rec *Record) bool {
	if s.budget == nil || ex.key == nil {
		return true
	}
	scopes := ex.key.Scopes()
	var amount metering.Nanos
	if !ex.route.Upstream.Free {
		if p, ok := s.prices.Lookup(ex.route.Model, ex.speed, ex.start); ok {
			amount = p.MaxCost(reserveTokens(len(ex.prompt)), ex.maxOutput)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, budgetTimeout)
	defer cancel()
	res, err := s.budget.Reserve(rctx, scopes, amount)
	var exceeded *metering.ExceededError
	switch {
	case err == nil:
		ex.reservation, rec.Reserved = res, amount
		return true
	case errors.As(err, &exceeded):
		rec.Status, rec.Error = http.StatusPaymentRequired, exceeded.Error()
		writeError(w, ex.format, http.StatusPaymentRequired,
			"Pitwall: "+exceeded.Error()+". Ask a Pitwall admin to raise the limit.")
		return false
	case s.cfg.FailOpen():
		s.log.Error("budget check failed; letting the call through (budget_fail_open)", "err", err, "key", ex.key.Prefix)
		ex.reservation = s.budget.Unreserved(scopes)
		return true
	default:
		rec.Status, rec.Error = http.StatusServiceUnavailable, err.Error()
		writeError(w, ex.format, http.StatusServiceUnavailable, "Pitwall could not check the budget; try again shortly")
		return false
	}
}

// priceCall sets the record's cost from its usage.
func (s *Server) priceCall(ex *exchange, rec *Record) {
	if ex.route.Upstream.Free {
		rec.Cost, rec.Priced = 0, true
		return
	}
	p, ok := s.prices.Lookup(ex.route.Model, rec.Usage.Speed, ex.start)
	rec.Priced = ok
	if !ok {
		rec.Cost = 0
		if _, warned := s.unpriced.LoadOrStore(ex.route.Model+"|"+rec.Usage.Speed, true); !warned {
			s.log.Warn("no price for model; its calls are recorded at $0 with priced=false",
				"model", ex.route.Model, "speed", rec.Usage.Speed, "provider", ex.route.Upstream.Name)
		}
		return
	}
	rec.Cost = p.Cost(rec.Usage)
	rec.CacheSavings = p.UncachedCost(rec.Usage) - rec.Cost
}

// assignTask places the call in its task. A failure is logged and the call
// goes on untracked: tracking must never block an agent.
func (s *Server) assignTask(ctx context.Context, ex *exchange, rec *Record) {
	var keyID int64
	if ex.key != nil {
		keyID = ex.key.ID
	}
	actx, cancel := context.WithTimeout(ctx, budgetTimeout)
	defer cancel()
	a, err := s.tracker.Assign(actx, tasks.Request{KeyID: keyID, Header: ex.taskHeader, Conv: ex.conv})
	if err != nil {
		s.log.Warn("task assignment incomplete", "err", err, "id", ex.id)
	}
	ex.assignment = a
	rec.TaskID, rec.TaskSource, rec.TaskExternalID = a.TaskID, a.Source, a.ExternalID
	rec.InferredTaskID, rec.CallIndex, rec.TaskStart = a.InferredTaskID, a.CallIndex, a.TaskStart
	rec.NewMessages = ex.conv.NewMessages(a.SharedPieces)
}

// finish prices the call and settles its reservation.
func (s *Server) finish(ctx context.Context, ex *exchange, rec *Record) {
	s.priceCall(ex, rec)
	rec.RepeatedInputTokens = ex.assignment.RepeatedTokens(rec.Usage.PromptTokens())
	if ex.reservation == nil || s.budget == nil {
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budgetTimeout)
	defer cancel()
	if err := s.budget.Settle(sctx, ex.reservation, rec.Cost); err != nil {
		s.log.Error("budget settle failed; this call's spend is in the request log but not the live counter",
			"err", err, "id", ex.id, "cost_usd", rec.Cost.USD())
	}
}
