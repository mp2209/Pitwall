package mockprovider

import (
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// The mock keeps its own loose request types rather than importing the
// gateway's, so a bug in the gateway's types cannot hide in both places.
type oaRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	Stream        bool `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

func oaText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	out := ""
	for _, p := range parts {
		out += p.Text
	}
	return out
}

func (s *Server) handleOpenAI(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	var req oaRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" || len(req.Messages) == 0 {
		oaError(w, http.StatusBadRequest, "invalid_request_error", "model and messages are required")
		return
	}
	b := s.behavior(r, string(body))
	if err := sleep(r.Context(), b.delay); err != nil {
		return
	}
	if b.errStatus != 0 {
		oaError(w, b.errStatus, "server_error", "mock provider injected error")
		return
	}

	// Simulated automatic prefix caching at message boundaries.
	h := newPrefixHasher()
	for _, t := range req.Tools {
		h.add([]byte(t.Function.Name))
		h.add(t.Function.Parameters)
	}
	var points []point
	lastUser := ""
	for _, m := range req.Messages {
		h.add([]byte(m.Role))
		h.add(m.Content)
		points = append(points, h.point())
		if m.Role == "user" {
			lastUser = oaText(m.Content)
		}
	}
	prompt := tokens(h.bytes)
	cached, _ := s.cache.lookup(points, s.cfg.MinCacheTokens)
	cached = cached / 128 * 128

	tools := make([]toolDef, len(req.Tools))
	for i, t := range req.Tools {
		tools[i] = toolDef{name: t.Function.Name, schema: t.Function.Parameters}
	}
	n := s.seq.Add(1)
	rep := decide(b.mode, tools, req.Messages[len(req.Messages)-1].Role == "tool", lastUser, n)
	output := tokens(len(rep.text) + len(rep.toolName) + len(rep.toolArgs))
	usage := map[string]any{
		"prompt_tokens":         prompt,
		"completion_tokens":     output,
		"total_tokens":          prompt + output,
		"prompt_tokens_details": map[string]any{"cached_tokens": cached},
	}
	id := "chatcmpl-mock" + itoa(n)
	created := time.Now().Unix()
	finish := "stop"
	if rep.isTool() {
		finish = "tool_calls"
	}

	if !req.Stream {
		msg := map[string]any{"role": "assistant", "content": nil}
		if rep.isTool() {
			msg["tool_calls"] = []map[string]any{{
				"id": rep.toolID, "type": "function",
				"function": map[string]any{"name": rep.toolName, "arguments": rep.toolArgs},
			}}
		} else {
			msg["content"] = rep.text
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
			"usage":   usage,
		})
		return
	}

	fw := startStream(w)
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	send := func(v map[string]any) bool {
		if err := sleep(r.Context(), b.chunkDelay); err != nil {
			return false
		}
		return fw.event("", v) == nil
	}
	if !send(chunk(map[string]any{"role": "assistant", "content": ""}, nil)) {
		return
	}
	if rep.isTool() {
		if !send(chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": rep.toolID, "type": "function",
			"function": map[string]any{"name": rep.toolName, "arguments": ""},
		}}}, nil)) {
			return
		}
		half := len(rep.toolArgs) / 2
		for _, piece := range []string{rep.toolArgs[:half], rep.toolArgs[half:]} {
			if !send(chunk(map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "function": map[string]any{"arguments": piece},
			}}}, nil)) {
				return
			}
		}
	} else {
		for _, word := range words(rep.text) {
			if !send(chunk(map[string]any{"content": word}, nil)) {
				return
			}
		}
	}
	if !send(chunk(map[string]any{}, finish)) {
		return
	}
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		if !send(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{}, "usage": usage,
		}) {
			return
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = fw.rc.Flush()
}

func oaError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": nil}})
}
