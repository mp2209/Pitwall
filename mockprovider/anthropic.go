package mockprovider

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

type anRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name         string          `json:"name"`
		InputSchema  json.RawMessage `json:"input_schema"`
		CacheControl json.RawMessage `json:"cache_control"`
	} `json:"tools"`
	Stream bool `json:"stream"`
}

type anBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	CacheControl json.RawMessage `json:"cache_control"`
}

// anBlocks splits string-or-array content into raw blocks.
func anBlocks(raw json.RawMessage) ([]json.RawMessage, []anBlock) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []json.RawMessage{raw}, []anBlock{{Type: "text", Text: s}}
	}
	var raws []json.RawMessage
	_ = json.Unmarshal(raw, &raws)
	blocks := make([]anBlock, len(raws))
	for i, r := range raws {
		_ = json.Unmarshal(r, &blocks[i])
	}
	return raws, blocks
}

func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	var req anRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" || len(req.Messages) == 0 {
		anError(w, http.StatusBadRequest, "model and messages are required")
		return
	}
	if req.MaxTokens <= 0 {
		anError(w, http.StatusBadRequest, "max_tokens: Field required")
		return
	}
	b := s.behavior(r, string(body))
	if err := sleep(r.Context(), b.delay); err != nil {
		return
	}
	if b.errStatus != 0 {
		anError(w, b.errStatus, "mock provider injected error")
		return
	}

	// Simulated prompt caching, after Anthropic's rules: every block boundary
	// is a position (a run of tool_use or of tool_result blocks counts as
	// one), entries are written at cache_control breakpoints, and each
	// breakpoint looks back up to 20 positions for an earlier entry. Markers
	// are stripped before hashing: moving a marker doesn't change content.
	h := newPrefixHasher()
	var positions []point
	var breakpoints []int
	for _, t := range req.Tools {
		h.add([]byte(t.Name))
		h.add(t.InputSchema)
		positions = append(positions, h.point())
		if len(t.CacheControl) > 0 {
			breakpoints = append(breakpoints, len(positions)-1)
		}
	}
	addBlocks := func(raw json.RawMessage) []anBlock {
		raws, blocks := anBlocks(raw)
		prevType := ""
		for i, rb := range raws {
			h.add(withoutCacheControl(rb))
			typ := blocks[i].Type
			if (typ == "tool_use" || typ == "tool_result") && typ == prevType {
				positions[len(positions)-1] = h.point() // same run, same position
			} else {
				positions = append(positions, h.point())
			}
			prevType = typ
			if len(blocks[i].CacheControl) > 0 {
				breakpoints = append(breakpoints, len(positions)-1)
			}
		}
		return blocks
	}
	if len(req.System) > 0 {
		addBlocks(req.System)
	}
	lastUser, lastIsToolResult, assistantTurns := "", false, 0
	for _, m := range req.Messages {
		h.add([]byte(m.Role))
		blocks := addBlocks(m.Content)
		switch m.Role {
		case "assistant":
			assistantTurns++
		case "user":
			lastIsToolResult = false
			for _, bl := range blocks {
				switch bl.Type {
				case "tool_result":
					lastIsToolResult = true
				case "text":
					lastUser = bl.Text
				}
			}
		}
	}
	if req.Messages[len(req.Messages)-1].Role != "user" {
		lastIsToolResult = false
	}
	total := tokens(h.bytes)
	read, written := s.cache.lookupBreakpoints(positions, breakpoints, s.cfg.MinCacheTokens)
	input := max(total-read-written, 0)

	tools := make([]toolDef, len(req.Tools))
	for i, t := range req.Tools {
		tools[i] = toolDef{name: t.Name, schema: t.InputSchema}
	}
	n := s.seq.Add(1)
	rep := decide(b, tools, lastIsToolResult, lastUser, assistantTurns, n)
	rep.toolID = "toolu_mock_" + itoa(n)
	output := tokens(len(rep.text) + len(rep.toolName) + len(rep.toolArgs))
	stop := "end_turn"
	if rep.isTool() {
		stop = "tool_use"
	}
	id := "msg_mock" + itoa(n)
	usage := map[string]any{
		"input_tokens":                input,
		"cache_creation_input_tokens": written,
		"cache_read_input_tokens":     read,
		"output_tokens":               output,
	}

	if !req.Stream {
		var content []map[string]any
		if rep.isTool() {
			content = []map[string]any{{"type": "tool_use", "id": rep.toolID, "name": rep.toolName, "input": json.RawMessage(rep.toolArgs)}}
		} else {
			content = []map[string]any{{"type": "text", "text": rep.text}}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage,
		})
		return
	}

	fw := startStream(w)
	send := func(name string, v map[string]any) bool {
		if err := sleep(r.Context(), b.chunkDelay); err != nil {
			return false
		}
		v["type"] = name
		return fw.event(name, v) == nil
	}
	startUsage := map[string]any{
		"input_tokens": input, "cache_creation_input_tokens": written,
		"cache_read_input_tokens": read, "output_tokens": 1,
	}
	if !send("message_start", map[string]any{"message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": startUsage,
	}}) {
		return
	}
	if !send("ping", map[string]any{}) {
		return
	}
	if rep.isTool() {
		if !send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{
			"type": "tool_use", "id": rep.toolID, "name": rep.toolName, "input": map[string]any{},
		}}) {
			return
		}
		half := len(rep.toolArgs) / 2
		for _, piece := range []string{rep.toolArgs[:half], rep.toolArgs[half:]} {
			if !send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": piece}}) {
				return
			}
		}
	} else {
		if !send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}}) {
			return
		}
		for _, word := range words(rep.text) {
			if !send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": word}}) {
				return
			}
		}
	}
	if !send("content_block_stop", map[string]any{"index": 0}) {
		return
	}
	if !send("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": output},
	}) {
		return
	}
	send("message_stop", map[string]any{})
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens(len(body))})
}

func anError(w http.ResponseWriter, status int, msg string) {
	typ := "api_error"
	switch status {
	case 400:
		typ = "invalid_request_error"
	case 429:
		typ = "rate_limit_error"
	case 529:
		typ = "overloaded_error"
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// withoutCacheControl drops a block's cache_control marker before hashing.
// Every object block is re-serialized the same way (sorted keys), marked or
// not, so the same content always hashes the same.
func withoutCacheControl(raw json.RawMessage) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	delete(m, "cache_control")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
