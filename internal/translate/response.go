package translate

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

// AnthropicToOpenAIResponse converts a Messages response to a chat
// completion. Thinking blocks are dropped. model is the name the client
// asked for, so the response names the model the client knows.
func AnthropicToOpenAIResponse(in *anthropic.MessagesResponse, model string) *openai.ChatResponse {
	msg := openai.ResponseMessage{Role: "assistant"}
	if text := anthropic.BlocksText(in.Content, ""); text != "" {
		msg.Content = &text
	}
	for _, b := range in.Content {
		if b.Type != "tool_use" {
			continue
		}
		args := string(b.Input)
		if args == "" {
			args = "{}"
		}
		msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
			ID:       b.ID,
			Type:     "function",
			Function: openai.FunctionCall{Name: b.Name, Arguments: args},
		})
	}
	stop := ""
	if in.StopReason != nil {
		stop = *in.StopReason
	}
	return &openai.ChatResponse{
		ID:      "chatcmpl-" + strings.TrimPrefix(in.ID, "msg_"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.Choice{{Index: 0, Message: msg, FinishReason: FinishReason(stop)}},
		Usage:   openai.FromMetering(in.Usage.Metering()),
	}
}

// OpenAIToAnthropicResponse converts the first choice of a chat completion
// to a Messages response.
func OpenAIToAnthropicResponse(in *openai.ChatResponse, model string) *anthropic.MessagesResponse {
	out := &anthropic.MessagesResponse{
		ID:      "msg_" + strings.TrimPrefix(in.ID, "chatcmpl-"),
		Type:    "message",
		Role:    "assistant",
		Model:   model,
		Content: []anthropic.ContentBlock{},
	}
	stop := "end_turn"
	if len(in.Choices) > 0 {
		c := in.Choices[0]
		if c.Message.Content != nil && *c.Message.Content != "" {
			out.Content = append(out.Content, anthropic.ContentBlock{Type: "text", Text: *c.Message.Content})
		}
		for _, tc := range c.Message.ToolCalls {
			out.Content = append(out.Content, anthropic.ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: toolInput(tc.Function.Arguments),
			})
		}
		stop = StopReason(c.FinishReason)
	}
	out.StopReason = &stop
	if in.Usage != nil {
		out.Usage = anthropic.FromMetering(in.Usage.Metering())
	}
	return out
}

// FinishReason maps an Anthropic stop_reason to an OpenAI finish_reason.
func FinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, pause_turn
		return "stop"
	}
}

// StopReason maps an OpenAI finish_reason to an Anthropic stop_reason.
func StopReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// ErrorMessage pulls a human-readable message out of an error body in either
// format, falling back to the raw body.
func ErrorMessage(body []byte) string {
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &probe) == nil && len(probe.Error) > 0 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(probe.Error, &e) == nil && e.Message != "" {
			return e.Message
		}
		var s string
		if json.Unmarshal(probe.Error, &s) == nil && s != "" {
			return s
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if msg == "" {
		msg = "upstream provider error"
	}
	return msg
}

// AnthropicErrorType picks the Anthropic error type for an HTTP status.
func AnthropicErrorType(status int) string {
	switch status {
	case 400, 413, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402, 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 529, 503:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// OpenAIErrorType picks the OpenAI error type for an HTTP status.
func OpenAIErrorType(status int) string {
	switch status {
	case 400, 404, 413, 422:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 402, 403:
		return "permission_error"
	case 429:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}
