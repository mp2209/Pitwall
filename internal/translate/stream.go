package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
	"github.com/mp2209/pitwall/internal/sse"
)

// Stream turns upstream stream events into client stream events and keeps a
// running token count. The gateway uses one Stream per streamed call: a
// passthrough when the client and provider speak the same format, a
// translator when they don't.
type Stream interface {
	// Event handles one upstream event, writing zero or more client events.
	Event(ev *sse.Event, w io.Writer) error
	// End writes any closing events the client still needs once the
	// upstream stream has ended.
	End(w io.Writer) error
	// Usage returns the tokens counted so far. If the stream was cut short
	// or the provider sent no usage, the missing parts are estimated and
	// Estimated is set.
	Usage() metering.Usage
}

// counter tracks provider-reported usage and falls back to estimates.
type counter struct {
	usage       metering.Usage
	inputKnown  bool // provider reported input tokens
	outputKnown bool // provider reported the final output count
	promptBytes int
	outBytes    int
}

func (c *counter) result() metering.Usage {
	u := c.usage
	if !c.inputKnown {
		u.InputTokens = metering.EstimateTokens(c.promptBytes)
		u.CachedReadTokens, u.CacheWriteTokens = 0, 0
		u.Estimated = true
	}
	if !c.outputKnown {
		u.OutputTokens = max(u.OutputTokens, metering.EstimateTokens(c.outBytes))
		u.Estimated = true
	}
	return u
}

// --- same-format passthrough -------------------------------------------

// OpenAIPassthrough forwards an OpenAI-format stream unchanged. The gateway
// always asks the provider for a final usage chunk; if the client did not
// ask for one, stripUsage drops that chunk so the client sees what it
// expects.
type OpenAIPassthrough struct {
	counter
	stripUsage bool
}

func NewOpenAIPassthrough(promptBytes int, stripUsage bool) *OpenAIPassthrough {
	return &OpenAIPassthrough{counter: counter{promptBytes: promptBytes}, stripUsage: stripUsage}
}

func (p *OpenAIPassthrough) Event(ev *sse.Event, w io.Writer) error {
	if len(ev.Data) > 0 && string(ev.Data) != "[DONE]" {
		var chunk openai.ChatChunk
		if json.Unmarshal(ev.Data, &chunk) == nil {
			for _, c := range chunk.Choices {
				if c.Delta.Content != nil {
					p.outBytes += len(*c.Delta.Content)
				}
				for _, tc := range c.Delta.ToolCalls {
					p.outBytes += len(tc.Function.Name) + len(tc.Function.Arguments)
				}
			}
			if chunk.Usage != nil {
				p.usage = chunk.Usage.Metering()
				p.inputKnown, p.outputKnown = true, true
				if p.stripUsage && len(chunk.Choices) == 0 {
					return nil
				}
			}
		}
	}
	_, err := w.Write(ev.Raw)
	return err
}

func (p *OpenAIPassthrough) End(io.Writer) error   { return nil }
func (p *OpenAIPassthrough) Usage() metering.Usage { return p.result() }

// AnthropicPassthrough forwards an Anthropic-format stream unchanged.
type AnthropicPassthrough struct {
	counter
	raw anthropic.Usage
}

func NewAnthropicPassthrough(promptBytes int) *AnthropicPassthrough {
	return &AnthropicPassthrough{counter: counter{promptBytes: promptBytes}}
}

func (p *AnthropicPassthrough) Event(ev *sse.Event, w io.Writer) error {
	if len(ev.Data) > 0 {
		var e anthropic.StreamEvent
		if json.Unmarshal(ev.Data, &e) == nil {
			p.observe(&e)
		}
	}
	_, err := w.Write(ev.Raw)
	return err
}

func (p *AnthropicPassthrough) observe(e *anthropic.StreamEvent) {
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			p.raw = e.Message.Usage
			p.usage = p.raw.Metering()
			p.inputKnown = true
		}
	case "content_block_start":
		var b anthropic.ContentBlock
		if json.Unmarshal(e.ContentBlock, &b) == nil {
			p.outBytes += len(b.Name)
		}
	case "content_block_delta":
		var d anthropic.BlockDelta
		if json.Unmarshal(e.Delta, &d) == nil {
			p.outBytes += len(d.Text) + len(d.PartialJSON)
		}
	case "message_delta":
		if e.Usage != nil {
			p.raw.Merge(*e.Usage)
			p.usage = p.raw.Metering()
			p.outputKnown = true
		}
	}
}

func (p *AnthropicPassthrough) End(io.Writer) error   { return nil }
func (p *AnthropicPassthrough) Usage() metering.Usage { return p.result() }

// --- Anthropic provider, OpenAI client ----------------------------------

// AnthropicToOpenAIStream turns an Anthropic Messages stream into OpenAI
// chat completion chunks. Each tool_use block becomes a tool call with its
// own index; thinking deltas are dropped.
type AnthropicToOpenAIStream struct {
	counter
	model        string
	includeUsage bool
	id           string
	created      int64
	raw          anthropic.Usage
	toolIndex    map[int]int // Anthropic block index -> OpenAI tool index
	nextTool     int
	done         bool
}

func NewAnthropicToOpenAIStream(model string, includeUsage bool, promptBytes int) *AnthropicToOpenAIStream {
	return &AnthropicToOpenAIStream{
		counter:      counter{promptBytes: promptBytes},
		model:        model,
		includeUsage: includeUsage,
		id:           "chatcmpl-pitwall",
		created:      time.Now().Unix(),
		toolIndex:    map[int]int{},
	}
}

func (s *AnthropicToOpenAIStream) chunk(delta openai.Delta, finish *string) *openai.ChatChunk {
	return &openai.ChatChunk{
		ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
		Choices: []openai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}

func writeJSON(w io.Writer, event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return sse.Write(w, event, b)
}

func (s *AnthropicToOpenAIStream) Event(ev *sse.Event, w io.Writer) error {
	if len(ev.Data) == 0 || s.done {
		return nil
	}
	var e anthropic.StreamEvent
	if err := json.Unmarshal(ev.Data, &e); err != nil {
		return fmt.Errorf("decode anthropic stream event: %w", err)
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			s.id = "chatcmpl-" + strings.TrimPrefix(e.Message.ID, "msg_")
			s.raw = e.Message.Usage
			s.usage = s.raw.Metering()
			s.inputKnown = true
		}
		empty := ""
		return writeJSON(w, "", s.chunk(openai.Delta{Role: "assistant", Content: &empty}, nil))

	case "content_block_start":
		var b anthropic.ContentBlock
		if err := json.Unmarshal(e.ContentBlock, &b); err != nil || b.Type != "tool_use" || e.Index == nil {
			return nil
		}
		idx := s.nextTool
		s.nextTool++
		s.toolIndex[*e.Index] = idx
		s.outBytes += len(b.Name)
		return writeJSON(w, "", s.chunk(openai.Delta{ToolCalls: []openai.ToolCall{{
			Index: &idx, ID: b.ID, Type: "function",
			Function: openai.FunctionCall{Name: b.Name, Arguments: ""},
		}}}, nil))

	case "content_block_delta":
		var d anthropic.BlockDelta
		if err := json.Unmarshal(e.Delta, &d); err != nil {
			return nil
		}
		switch d.Type {
		case "text_delta":
			if d.Text == "" {
				return nil
			}
			s.outBytes += len(d.Text)
			text := d.Text
			return writeJSON(w, "", s.chunk(openai.Delta{Content: &text}, nil))
		case "input_json_delta":
			idx, ok := s.toolIndex[derefInt(e.Index)]
			if !ok || d.PartialJSON == "" {
				return nil
			}
			s.outBytes += len(d.PartialJSON)
			return writeJSON(w, "", s.chunk(openai.Delta{ToolCalls: []openai.ToolCall{{
				Index: &idx, Function: openai.FunctionCall{Arguments: d.PartialJSON},
			}}}, nil))
		}

	case "message_delta":
		if e.Usage != nil {
			s.raw.Merge(*e.Usage)
			s.usage = s.raw.Metering()
			s.outputKnown = true
		}
		var md anthropic.MessageDelta
		if json.Unmarshal(e.Delta, &md) == nil && md.StopReason != nil {
			finish := FinishReason(*md.StopReason)
			return writeJSON(w, "", s.chunk(openai.Delta{}, &finish))
		}

	case "message_stop":
		return s.finish(w)

	case "error":
		s.done = true
		body := openai.ErrorBody{Message: "upstream stream error", Type: "server_error"}
		if e.Error != nil {
			body.Message, body.Type = e.Error.Message, e.Error.Type
		}
		return writeJSON(w, "", openai.ErrorResponse{Error: body})
	}
	return nil
}

func (s *AnthropicToOpenAIStream) finish(w io.Writer) error {
	if s.done {
		return nil
	}
	s.done = true
	if s.includeUsage {
		c := &openai.ChatChunk{
			ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model,
			Choices: []openai.ChunkChoice{}, Usage: openai.FromMetering(s.result()),
		}
		if err := writeJSON(w, "", c); err != nil {
			return err
		}
	}
	return sse.Write(w, "", []byte("[DONE]"))
}

func (s *AnthropicToOpenAIStream) End(w io.Writer) error { return s.finish(w) }
func (s *AnthropicToOpenAIStream) Usage() metering.Usage { return s.result() }

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// --- OpenAI provider, Anthropic client ----------------------------------

// OpenAIToAnthropicStream turns OpenAI chat completion chunks into an
// Anthropic Messages stream: one text block per run of content deltas and
// one tool_use block per tool call. Usage goes in the final message_delta,
// since OpenAI only reports it at the end.
type OpenAIToAnthropicStream struct {
	counter
	model     string
	id        string
	started   bool
	blockOpen bool
	openType  string // "text" or "tool_use"
	nextBlock int
	toolBlock map[int]int // OpenAI tool index -> Anthropic block index
	finish    string
	done      bool
}

func NewOpenAIToAnthropicStream(model string, promptBytes int) *OpenAIToAnthropicStream {
	return &OpenAIToAnthropicStream{
		counter:   counter{promptBytes: promptBytes},
		model:     model,
		id:        "msg_pitwall",
		toolBlock: map[int]int{},
	}
}

func (s *OpenAIToAnthropicStream) start(w io.Writer) error {
	if s.started {
		return nil
	}
	s.started = true
	msg := anthropic.MessagesResponse{
		ID: s.id, Type: "message", Role: "assistant", Model: s.model,
		Content: []anthropic.ContentBlock{}, Usage: anthropic.Usage{},
	}
	return writeJSON(w, "message_start", map[string]any{"type": "message_start", "message": msg})
}

func (s *OpenAIToAnthropicStream) closeBlock(w io.Writer) error {
	if !s.blockOpen {
		return nil
	}
	s.blockOpen = false
	return writeJSON(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.nextBlock - 1})
}

func (s *OpenAIToAnthropicStream) openBlock(w io.Writer, kind string, block any) (int, error) {
	if err := s.closeBlock(w); err != nil {
		return 0, err
	}
	idx := s.nextBlock
	s.nextBlock++
	s.blockOpen, s.openType = true, kind
	return idx, writeJSON(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": block,
	})
}

func (s *OpenAIToAnthropicStream) Event(ev *sse.Event, w io.Writer) error {
	if len(ev.Data) == 0 || s.done {
		return nil
	}
	if string(ev.Data) == "[DONE]" {
		return s.finishStream(w)
	}
	var chunk openai.ChatChunk
	if err := json.Unmarshal(ev.Data, &chunk); err != nil {
		return fmt.Errorf("decode openai stream chunk: %w", err)
	}
	if chunk.Error != nil {
		s.done = true
		return writeJSON(w, "error", anthropic.ErrorResponse{
			Type:  "error",
			Error: anthropic.ErrorBody{Type: "api_error", Message: chunk.Error.Message},
		})
	}
	if !s.started && chunk.ID != "" {
		s.id = "msg_" + strings.TrimPrefix(chunk.ID, "chatcmpl-")
	}
	if err := s.start(w); err != nil {
		return err
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage.Metering()
		s.inputKnown, s.outputKnown = true, true
	}
	for _, c := range chunk.Choices {
		if c.Index != 0 {
			continue
		}
		if err := s.delta(w, c.Delta); err != nil {
			return err
		}
		if c.FinishReason != nil && *c.FinishReason != "" {
			s.finish = *c.FinishReason
		}
	}
	return nil
}

func (s *OpenAIToAnthropicStream) delta(w io.Writer, d openai.Delta) error {
	if d.Content != nil && *d.Content != "" {
		if !s.blockOpen || s.openType != "text" {
			if _, err := s.openBlock(w, "text", map[string]string{"type": "text", "text": ""}); err != nil {
				return err
			}
		}
		s.outBytes += len(*d.Content)
		if err := writeJSON(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.nextBlock - 1,
			"delta": map[string]string{"type": "text_delta", "text": *d.Content},
		}); err != nil {
			return err
		}
	}
	for i, tc := range d.ToolCalls {
		// Some OpenAI-compatible servers (Ollama among them) omit the index
		// and send each call whole; treat every such call as a new one.
		oi := len(s.toolBlock) + 1000 + i
		if tc.Index != nil {
			oi = *tc.Index
		}
		block, known := s.toolBlock[oi]
		if !known || (tc.ID != "" && tc.Index == nil) {
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("toolu_pitwall_%d", oi)
			}
			var err error
			block, err = s.openBlock(w, "tool_use", map[string]any{
				"type": "tool_use", "id": id, "name": tc.Function.Name, "input": map[string]any{},
			})
			if err != nil {
				return err
			}
			s.toolBlock[oi] = block
			s.outBytes += len(tc.Function.Name)
		}
		if tc.Function.Arguments == "" {
			continue
		}
		s.outBytes += len(tc.Function.Arguments)
		if err := writeJSON(w, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": block,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *OpenAIToAnthropicStream) finishStream(w io.Writer) error {
	if s.done {
		return nil
	}
	s.done = true
	if err := s.start(w); err != nil {
		return err
	}
	if err := s.closeBlock(w); err != nil {
		return err
	}
	stop := StopReason(s.finish)
	if err := writeJSON(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": anthropic.FromMetering(s.result()),
	}); err != nil {
		return err
	}
	return writeJSON(w, "message_stop", map[string]string{"type": "message_stop"})
}

func (s *OpenAIToAnthropicStream) End(w io.Writer) error { return s.finishStream(w) }
func (s *OpenAIToAnthropicStream) Usage() metering.Usage { return s.result() }
