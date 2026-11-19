package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mp2209/pitwall/internal/store"
	"github.com/mp2209/pitwall/internal/tasks"
)

// handleTaskOutcome lets an agent or benchmark runner report how a task
// ended (tests passed or not), which is what turns cost per task into cost
// per solved task. The body names the task by the label the client sent in
// x-pitwall-task, or by the ID the gateway returned in x-pitwall-task-id:
//
//	{"task": "fix-parser-1", "outcome": "solved"}
//	{"task_id": "ti_3f2a...", "outcome": "failed"}
func (s *Server) handleTaskOutcome(w http.ResponseWriter, r *http.Request) {
	key := keyFrom(r.Context())
	if s.store == nil || key == nil {
		adminError(w, http.StatusNotImplemented, "task outcomes need a database (PITWALL_DATABASE_URL)")
		return
	}
	var in struct {
		Task    string `json:"task"`
		TaskID  string `json:"task_id"`
		Outcome string `json:"outcome"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !store.ValidOutcome(in.Outcome) {
		adminError(w, http.StatusBadRequest, `outcome must be "solved", "failed", "stopped" or "unknown"`)
		return
	}
	ref := store.TaskRef{KeyID: key.ID}
	if key.TeamID != 0 {
		team := key.TeamID
		ref.TeamID = &team
	}
	switch label := tasks.CleanLabel(in.Task); {
	case label != "":
		ref.ID, ref.Source, ref.ExternalID = tasks.HeaderTaskID(key.ID, label), tasks.SourceHeader, label
	case strings.HasPrefix(in.TaskID, "ti_"):
		ref.ID, ref.Source = in.TaskID, tasks.SourceInferred
	case strings.HasPrefix(in.TaskID, "t_"):
		ref.ID, ref.Source = in.TaskID, tasks.SourceHeader
	default:
		adminError(w, http.StatusBadRequest, `give "task" (your x-pitwall-task label) or "task_id" (from x-pitwall-task-id)`)
		return
	}
	if err := s.store.SetTaskOutcome(r.Context(), ref, in.Outcome); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			adminError(w, http.StatusNotFound, "no such task for this key")
			return
		}
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"task_id": ref.ID, "outcome": in.Outcome})
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TaskFilter{ConfigLabel: q.Get("config_label"), Outcome: q.Get("outcome"), Source: q.Get("source")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if v := q.Get("key_id"); v != "" {
		var err error
		if f.KeyID, err = strconv.ParseInt(v, 10, 64); err != nil {
			adminError(w, http.StatusBadRequest, "key_id must be an integer")
			return
		}
	}
	list, err := s.store.ListTasks(r.Context(), f)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": list})
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	t, calls, err := s.store.TaskWithCalls(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "calls": calls})
}
