package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mp2209/pitwall/internal/metering"
)

// Sources of a task.
const (
	SourceHeader   = "header"
	SourceInferred = "inferred"
)

// Headers clients use to describe their task.
const (
	HeaderTask   = "x-pitwall-task"   // the client's name for this agent run
	HeaderAgent  = "x-pitwall-agent"  // which harness is calling
	HeaderConfig = "x-pitwall-config" // label for the configuration under test
	// ResponseTaskID tells the client which task its call was recorded under.
	ResponseTaskID = "x-pitwall-task-id"
)

// Store keeps task state between calls. Update must apply fn atomically:
// fn receives the current value (nil if none) and returns the new one, which
// is kept for ttl. fn may run more than once if another writer races it.
type Store interface {
	Update(ctx context.Context, key string, ttl time.Duration, fn func(old []byte) ([]byte, error)) error
	// Get returns the current value, or nil if there is none.
	Get(ctx context.Context, key string) ([]byte, error)
}

// Tracker assigns calls to tasks.
type Tracker struct {
	store Store
	// IdleGap closes an inferred task: a call after this long without one
	// starts a new task.
	IdleGap time.Duration
	// HeaderTTL is how long a header task's state is kept between calls.
	HeaderTTL time.Duration
	now       func() time.Time
}

// NewTracker returns a tracker with a 5-minute idle gap.
func NewTracker(store Store) *Tracker {
	return &Tracker{store: store, IdleGap: 5 * time.Minute, HeaderTTL: 24 * time.Hour, now: time.Now}
}

// Request is one call to assign.
type Request struct {
	KeyID  int64
	Header string // the x-pitwall-task value, or empty
	Conv   Conversation
}

// Assignment is where a call belongs.
type Assignment struct {
	TaskID     string // the task the call is recorded under
	Source     string
	ExternalID string // the header value, for header tasks
	// InferredTaskID is what inference chose, also when a header named the
	// task, so inference can be scored against the header.
	InferredTaskID string
	CallIndex      int // 1 for a task's first call; 0 if unknown
	// TaskStart is when the task's first call arrived (zero if unknown), so
	// a trace can be created with the same start time from any call.
	TaskStart time.Time
	// SharedPieces counts the pieces identical to the task's previous call;
	// RepeatedBytes is their size and TotalBytes the whole prompt's.
	SharedPieces  int
	RepeatedBytes int
	TotalBytes    int
}

// RepeatedTokens scales a call's prompt token count by the share of its
// prompt bytes that repeat the previous call.
func (a Assignment) RepeatedTokens(promptTokens int64) int64 {
	if a.TotalBytes == 0 || a.RepeatedBytes == 0 {
		return 0
	}
	return promptTokens * int64(a.RepeatedBytes) / int64(a.TotalBytes)
}

// HeaderTaskID derives a header task's ID from the key and the client's
// label, so every gateway replica agrees without coordination and two keys
// using the same label get different tasks.
func HeaderTaskID(keyID int64, label string) string {
	sum := sha256.Sum256([]byte(strconv.FormatInt(keyID, 10) + "\x00" + label))
	return "t_" + hex.EncodeToString(sum[:10])
}

func newInferredID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "ti_" + hex.EncodeToString(b[:])
}

// CleanLabel makes a client-supplied header value safe to store: trimmed,
// without control characters, at most 200 bytes.
func CleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	for len(s) > 200 {
		_, size := lastRune(s)
		s = s[:len(s)-size]
	}
	return s
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, 1
}

// AgentName picks a name for the calling harness: the x-pitwall-agent
// header, or the product name from the User-Agent ("claude-cli/2.1 ..." ->
// "claude-cli").
func AgentName(header, userAgent string) string {
	if a := CleanLabel(header); a != "" {
		return a
	}
	product, _, _ := strings.Cut(strings.TrimSpace(userAgent), " ")
	product, _, _ = strings.Cut(product, "/")
	product = strings.ToLower(CleanLabel(product))
	if len(product) > 40 {
		product = product[:40]
	}
	return product
}

// convState is what is kept about a task between its calls.
type convState struct {
	ID     string   `json:"id"`
	Start  int64    `json:"start"` // unix milliseconds of the first call
	Last   int64    `json:"last"`  // unix milliseconds of the latest call
	N      int      `json:"n"`     // message count of the latest call
	Calls  int      `json:"calls"` // calls so far
	Prefix []uint64 `json:"prefix"`
}

// maxOpen bounds the open conversations kept per key and opening.
const maxOpen = 8

func openKey(keyID int64, opening uint64) string {
	return fmt.Sprintf("pitwall:{tasks}:open:%d:%016x", keyID, opening)
}

func headerKey(taskID string) string { return "pitwall:{tasks}:hdr:" + taskID }

func costKey(taskID string) string { return "pitwall:{tasks}:cost:" + taskID }

// TaskCost returns what a task has cost so far, as recorded by AddTaskCost.
func (t *Tracker) TaskCost(ctx context.Context, taskID string) (metering.Nanos, error) {
	raw, err := t.store.Get(ctx, costKey(taskID))
	if err != nil || len(raw) == 0 {
		return 0, err
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	return metering.Nanos(n), err
}

// AddTaskCost adds a finished call's cost to its task's running total, which
// the guard's per-task cost cap reads before the next call.
func (t *Tracker) AddTaskCost(ctx context.Context, taskID string, cost metering.Nanos) error {
	return t.store.Update(ctx, costKey(taskID), t.HeaderTTL, func(old []byte) ([]byte, error) {
		n, _ := strconv.ParseInt(string(old), 10, 64)
		return []byte(strconv.FormatInt(n+int64(cost), 10)), nil
	})
}

// Assign places a call in a task. When the store fails, a header task is
// still assigned (its ID needs no state) and the error is returned
// alongside, so the call can go on.
func (t *Tracker) Assign(ctx context.Context, req Request) (Assignment, error) {
	a := Assignment{TotalBytes: req.Conv.TotalBytes()}
	now := t.now()
	inferredID, infShared, infCalls, infStart, inferErr := t.infer(ctx, req, now)
	a.InferredTaskID = inferredID

	label := CleanLabel(req.Header)
	if label == "" {
		if inferErr != nil {
			return Assignment{}, inferErr
		}
		a.TaskID, a.Source = inferredID, SourceInferred
		a.CallIndex, a.SharedPieces, a.TaskStart = infCalls, infShared, infStart
	} else {
		a.TaskID, a.Source, a.ExternalID = HeaderTaskID(req.KeyID, label), SourceHeader, label
		shared, calls, start, err := t.trackHeader(ctx, a.TaskID, req.Conv, now)
		if err != nil {
			return a, errors.Join(inferErr, err)
		}
		a.CallIndex, a.SharedPieces, a.TaskStart = calls, shared, start
		if inferErr != nil {
			return a, inferErr
		}
	}
	if a.SharedPieces > 0 {
		a.RepeatedBytes = req.Conv.Bytes[a.SharedPieces-1]
	}
	return a, nil
}

// infer finds the open conversation this call continues, or starts one.
func (t *Tracker) infer(ctx context.Context, req Request, now time.Time) (id string, shared, calls int, start time.Time, err error) {
	cur := req.Conv
	err = t.store.Update(ctx, openKey(req.KeyID, cur.Opening), t.IdleGap, func(old []byte) ([]byte, error) {
		var open []convState
		if len(old) > 0 {
			if err := json.Unmarshal(old, &open); err != nil {
				open = nil // corrupt state: start over rather than fail the call
			}
		}
		live := open[:0]
		for _, c := range open {
			if now.Sub(time.UnixMilli(c.Last)) <= t.IdleGap {
				live = append(live, c)
			}
		}
		open = live

		// A call shaped like a fresh start (one or two messages) can't continue
		// a conversation that already went further, nor one whose last call it
		// repeats exactly: after an answer, an agent's next call grows the
		// history, so an identical opening call is a new run of the same task
		// (a benchmark repeat, a second session). The cost: a retry of a
		// failed first call starts a task of its own, worth $0.
		restart := func(c convState) bool {
			if cur.Messages > 2 {
				return false
			}
			same := len(cur.Prefix) == len(c.Prefix) && len(c.Prefix) > 0 && cur.Prefix[len(cur.Prefix)-1] == c.Prefix[len(c.Prefix)-1]
			return cur.Messages < c.N || same
		}
		pick := -1
		// 1. The conversation whose previous call this one extends, preferring
		//    the longest match (the most advanced run), then the most recent.
		for i, c := range open {
			if restart(c) || !extends(cur.Prefix, c.Prefix) {
				continue
			}
			if pick < 0 || len(c.Prefix) > len(open[pick].Prefix) ||
				len(c.Prefix) == len(open[pick].Prefix) && c.Last > open[pick].Last {
				pick = i
			}
		}
		// 2. Same system prompt and opening turn but edited history (trimmed
		//    tool output, a moved reminder): the most recent conversation.
		if pick < 0 {
			for i, c := range open {
				if !restart(c) && (pick < 0 || c.Last > open[pick].Last) {
					pick = i
				}
			}
		}
		var c convState
		if pick >= 0 {
			c = open[pick]
			shared = sharedPrefix(cur.Prefix, c.Prefix)
		} else {
			c = convState{ID: newInferredID(), Start: now.UnixMilli()}
			shared = 0
			if len(open) >= maxOpen {
				open = open[1:]
			}
			open = append(open, c)
			pick = len(open) - 1
		}
		if c.Start == 0 { // state written before Start existed
			c.Start = now.UnixMilli()
		}
		c.Last, c.N, c.Prefix = now.UnixMilli(), cur.Messages, cur.Prefix
		c.Calls++
		open[pick] = c
		id, calls, start = c.ID, c.Calls, time.UnixMilli(c.Start)
		return json.Marshal(open)
	})
	if err != nil {
		return "", 0, 0, time.Time{}, fmt.Errorf("task inference: %w", err)
	}
	return id, shared, calls, start, nil
}

// trackHeader keeps a header task's previous call for the repeated-input
// count.
func (t *Tracker) trackHeader(ctx context.Context, taskID string, cur Conversation, now time.Time) (shared, calls int, start time.Time, err error) {
	err = t.store.Update(ctx, headerKey(taskID), t.HeaderTTL, func(old []byte) ([]byte, error) {
		var c convState
		if len(old) > 0 {
			_ = json.Unmarshal(old, &c)
		}
		shared = 0
		if c.Calls > 0 {
			shared = sharedPrefix(cur.Prefix, c.Prefix)
		}
		if c.Start == 0 {
			c.Start = now.UnixMilli()
		}
		c.ID, c.Last, c.N, c.Prefix = taskID, now.UnixMilli(), cur.Messages, cur.Prefix
		c.Calls++
		calls, start = c.Calls, time.UnixMilli(c.Start)
		return json.Marshal(c)
	})
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("task state: %w", err)
	}
	return shared, calls, start, nil
}
