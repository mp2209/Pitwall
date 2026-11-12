// Package openai holds the OpenAI Chat Completions wire format, which is also
// what Ollama's OpenAI-compatible endpoint speaks.
package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mp2209/pitwall/internal/metering"
)

// ChatRequest is the subset of a chat completion request the gateway
// translates. Same-format calls are forwarded as raw JSON instead, so fields
// missing here still reach the provider.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"` // string or []string
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"` // string or object
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	User                string          `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"` // string, []ContentPart or null
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type ToolCall struct {
	Index    *int         `json:"index,omitempty"` // set in stream deltas only
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

type ResponseMessage struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Usage struct {
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// Metering converts provider usage to the gateway's normalized form.
func (u *Usage) Metering() metering.Usage {
	if u == nil {
		return metering.Usage{}
	}
	var cached int64
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	return metering.Usage{
		InputTokens:      max(u.PromptTokens-cached, 0),
		CachedReadTokens: cached,
		OutputTokens:     u.CompletionTokens,
	}
}

// FromMetering builds OpenAI usage from normalized usage. Cache writes count
// as ordinary prompt tokens, since the format has no field for them.
func FromMetering(m metering.Usage) *Usage {
	return &Usage{
		PromptTokens:        m.PromptTokens(),
		CompletionTokens:    m.OutputTokens,
		TotalTokens:         m.PromptTokens() + m.OutputTokens,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: m.CachedReadTokens},
	}
}

type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
	Error   *ErrorBody    `json:"error,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   *string    `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code,omitempty"`
}

// ParseContent splits a message's content into plain text or parts. Exactly
// one of the results is set for non-empty content.
func ParseContent(raw json.RawMessage) (text string, parts []ContentPart, err error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", nil, nil
	}
	if s[0] == '"' {
		err = json.Unmarshal(raw, &text)
		return text, nil, err
	}
	if err = json.Unmarshal(raw, &parts); err != nil {
		return "", nil, fmt.Errorf("content must be a string or an array of parts: %w", err)
	}
	return "", parts, nil
}

// ContentText returns all text in a message's content, joining parts with
// newlines and ignoring non-text parts.
func ContentText(raw json.RawMessage) (string, error) {
	text, parts, err := ParseContent(raw)
	if err != nil || parts == nil {
		return text, err
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}
