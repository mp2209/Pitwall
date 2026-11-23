package api

import (
	"context"
	"net/http"

	"github.com/mp2209/pitwall/internal/guard"
	"github.com/mp2209/pitwall/internal/metering"
)

// HeaderWarning carries guard warnings to the client.
const HeaderWarning = "x-pitwall-warning"

// checkGuard runs the runaway-agent guard before a call. A warning adds a
// header and lets the call through; a stop refuses it with a 400, which
// SDKs and agents don't retry, and a message the agent shows its user.
func (s *Server) checkGuard(w http.ResponseWriter, ctx context.Context, ex *exchange, rec *Record) bool {
	if !s.guard.Enabled {
		return true
	}
	in := guard.Input{Loop: ex.conv.Loop, ContextTokens: metering.EstimateTokens(len(ex.prompt))}
	if limit := s.guard.TaskCostUSD; (limit.Warn > 0 || limit.Stop > 0) && ex.assignment.TaskID != "" {
		cctx, cancel := context.WithTimeout(ctx, budgetTimeout)
		cost, err := s.tracker.TaskCost(cctx, ex.assignment.TaskID)
		cancel()
		if err != nil {
			s.log.Warn("task cost unavailable; the cost cap is skipped for this call", "err", err, "task", ex.assignment.TaskID)
		}
		in.TaskCost = cost
	}
	v := s.guard.Check(in)
	if v.Action == guard.ActionNone {
		return true
	}
	rec.GuardEvents, rec.GuardAction = v.Events(), v.Action
	w.Header().Set(HeaderWarning, v.WarningHeader())
	if v.Action == guard.ActionWarn {
		return true
	}
	msg := v.StopMessage()
	rec.Status, rec.Error = http.StatusBadRequest, msg
	writeError(w, ex.format, http.StatusBadRequest, msg)
	return false
}
