// Package cacheopt adds prompt-cache breakpoints to Anthropic requests that
// don't have any.
//
// Agents resend the same tools, system prompt and conversation on every
// turn. Anthropic caches a prefix only where the request marks it with
// cache_control, and many clients never add the marks, so they pay full
// input price for the same tokens turn after turn. The optimizer places the
// two breakpoints Anthropic's guidance recommends for agent loops:
//
//  1. on the last system block (or the last tool when there is no system
//     prompt), which caches tools and system together; this survives any
//     change later in the conversation;
//  2. on the last block of the newest turn, so the next call reads the whole
//     conversation so far and only writes what it adds.
//
// A cache write costs 1.25x plain input and a read 0.025x to 0.1x, so a
// marked prefix pays off once it is read again. Prefixes shorter than the
// model's minimum are never cached, so no marker is placed there. Requests
// that already carry any cache_control are left alone: the client manages
// its own caching.
package cacheopt

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/mp2209/pitwall/internal/metering"
)

// Outcomes, recorded per call.
const (
	Applied       = "applied"        // at least one breakpoint added
	ClientManaged = "client_managed" // the request already had markers
	TooShort      = "too_short"      // below the model's minimum cacheable length
	NotApplicable = "not_applicable" // nothing that can carry a marker
)

// Result says what the optimizer did to one request.
type Result struct {
	Added   int
	Outcome string
}

// minimums is the smallest cacheable prefix per model family, longest
// prefix match first. From Anthropic's prompt caching documentation; the
// minimum is not monotonic across generations.
var minimums = []struct {
	prefix string
	tokens int64
}{
	{"claude-opus-4-5", 4096}, {"claude-opus-4-6", 4096}, {"claude-haiku-4-5", 4096},
	{"claude-opus-4-7", 2048}, {"claude-mythos-preview", 2048}, {"claude-3-5-haiku", 2048}, {"claude-haiku-3-5", 2048},
	{"claude-opus-4-8", 1024}, {"claude-sonnet-4-6", 1024}, {"claude-sonnet-4-5", 1024},
	{"claude-opus-4-1", 1024}, {"claude-opus-4", 1024}, {"claude-sonnet-4", 1024},
	{"claude-sonnet-5-5", 512}, {"claude-haiku-5-5", 512}, {"claude-sonnet-5", 1024},
	{"claude-opus-5", 512}, {"claude-fable-5", 512}, {"claude-mythos-5", 512},
}

// DefaultMinimum applies to models not in the table (including the mock).
const DefaultMinimum int64 = 1024

// MinCacheTokens returns the minimum cacheable prefix for a model.
func MinCacheTokens(model string) int64 {
	best, bestLen := DefaultMinimum, -1
	for _, m := range minimums {
		if strings.HasPrefix(model, m.prefix) && len(m.prefix) > bestLen {
			best, bestLen = m.tokens, len(m.prefix)
		}
	}
	return best
}

var marker = json.RawMessage(`{"type":"ephemeral"}`)

// blocks that accept cache_control.
var markable = map[string]bool{"text": true, "image": true, "tool_use": true, "tool_result": true, "document": true}

// Apply adds breakpoints to an Anthropic Messages request body for model.
// It changes nothing and returns the body as is when it can't help.
func Apply(body []byte, model string) ([]byte, Result, error) {
	if bytes.Contains(body, []byte(`"cache_control"`)) {
		return body, Result{Outcome: ClientManaged}, nil
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, Result{}, err
	}
	minTokens := MinCacheTokens(model)
	res := Result{Outcome: NotApplicable}
	tooShort := false

	// 1. Tools and system: worth a marker only if their prefix is cacheable.
	if metering.EstimateTokens(len(req["tools"])+len(req["system"])) >= minTokens {
		if sys, ok := markSystem(req["system"]); ok {
			req["system"] = sys
			res.Added++
		} else if tools, ok := markLast(req["tools"], func(map[string]json.RawMessage) bool { return true }); ok {
			req["tools"] = tools
			res.Added++
		}
	} else if len(req["tools"])+len(req["system"]) > 0 {
		tooShort = true
	}

	// 2. The newest turn, if the whole prompt is cacheable.
	if metering.EstimateTokens(len(body)) >= minTokens {
		if msgs, ok := markNewestTurn(req["messages"]); ok {
			req["messages"] = msgs
			res.Added++
		}
	} else {
		tooShort = true
	}

	switch {
	case res.Added > 0:
		res.Outcome = Applied
	case tooShort:
		res.Outcome = TooShort
	}
	if res.Added == 0 {
		return body, res, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, Result{}, err
	}
	return out, res, nil
}

// markSystem marks the last non-empty text block of the system prompt,
// turning a plain-string system prompt into a one-block array.
func markSystem(raw json.RawMessage) (json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return nil, false
		}
		out, err := json.Marshal([]map[string]any{{"type": "text", "text": s, "cache_control": marker}})
		return out, err == nil
	}
	return markLast(raw, eligibleBlock)
}

// markNewestTurn marks the last eligible block of the newest turn. A trailing
// mid-conversation system message can't carry a marker, so the walk moves
// back to the turn before it.
func markNewestTurn(raw json.RawMessage) (json.RawMessage, bool) {
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		var role string
		_ = json.Unmarshal(msgs[i]["role"], &role)
		if role == "system" {
			continue
		}
		content := bytes.TrimSpace(msgs[i]["content"])
		var marked json.RawMessage
		var ok bool
		if len(content) > 0 && content[0] == '"' {
			var s string
			if json.Unmarshal(content, &s) == nil && s != "" {
				marked, _ = json.Marshal([]map[string]any{{"type": "text", "text": s, "cache_control": marker}})
				ok = true
			}
		} else {
			marked, ok = markLast(content, eligibleBlock)
		}
		if !ok {
			return nil, false // the newest turn has nothing markable; don't reach further back
		}
		msgs[i]["content"] = marked
		out, err := json.Marshal(msgs)
		return out, err == nil
	}
	return nil, false
}

// eligibleBlock reports whether a content block can carry cache_control:
// thinking blocks can't, and neither can empty text.
func eligibleBlock(b map[string]json.RawMessage) bool {
	var typ, text string
	_ = json.Unmarshal(b["type"], &typ)
	if !markable[typ] {
		return false
	}
	if typ == "text" {
		_ = json.Unmarshal(b["text"], &text)
		return text != ""
	}
	return true
}

// markLast adds cache_control to the last object in a JSON array that ok
// accepts, keeping every other field as it was.
func markLast(raw json.RawMessage, ok func(map[string]json.RawMessage) bool) (json.RawMessage, bool) {
	var items []map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i] != nil && ok(items[i]) {
			items[i]["cache_control"] = marker
			out, err := json.Marshal(items)
			return out, err == nil
		}
	}
	return nil, false
}
