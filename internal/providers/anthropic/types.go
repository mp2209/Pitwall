// Package anthropic holds the Anthropic Messages API wire format.
package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mp2209/pitwall/internal/metering"
)

// DefaultVersion is sent as anthropic-version when the client sends none.
const DefaultVersion = "2023-06-01"

// MessagesRequest is the subset of a Messages request the gateway translates.
// Same-format calls are forwarded as raw JSON instead.
type MessagesRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	System        json.RawMessage `json:"system,omitempty"` // string or []ContentBlock
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Metadata      *Metadata       `json:"metadata,omitempty"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []ContentBlock
}

// ContentBlock covers the block types the gateway translates: text, image,
// tool_use, tool_result, thinking and redacted_thinking.
type ContentBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"` // text

	Source *ImageSource `json:"source,omitempty"` // image

	ID    string          `json:"id,omitempty"`    // tool_use
	Name  string          `json:"name,omitempty"`  // tool_use
	Input json.RawMessage `json:"input,omitempty"` // tool_use

	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result: string or []ContentBlock
	IsError   bool            `json:"is_error,omitempty"`    // tool_result

	Thinking  string `json:"thinking,omitempty"`  // thinking
	Signature string `json:"signature,omitempty"` // thinking

	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"` // base64 or url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type Tool struct {
	Type         string          `json:"type,omitempty"` // empty or "custom" for client tools
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

type ToolChoice struct {
	Type                   string `json:"type"` // auto, any, tool, none
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   *string        `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}

type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

// Metering converts provider usage to the gateway's normalized form.
func (u Usage) Metering() metering.Usage {
	return metering.Usage{
		InputTokens:      u.InputTokens,
		CachedReadTokens: u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		OutputTokens:     u.OutputTokens,
	}
}

// Merge folds a later usage report (message_delta) into an earlier one
// (message_start). message_delta carries the cumulative output count and,
// on newer API versions, the input counts too.
func (u *Usage) Merge(later Usage) {
	if later.InputTokens > 0 {
		u.InputTokens = later.InputTokens
	}
	if later.CacheCreationInputTokens > 0 {
		u.CacheCreationInputTokens = later.CacheCreationInputTokens
	}
	if later.CacheReadInputTokens > 0 {
		u.CacheReadInputTokens = later.CacheReadInputTokens
	}
	if later.OutputTokens > 0 {
		u.OutputTokens = later.OutputTokens
	}
}

// FromMetering builds Anthropic usage from normalized usage.
func FromMetering(m metering.Usage) Usage {
	return Usage{
		InputTokens:              m.InputTokens,
		OutputTokens:             m.OutputTokens,
		CacheCreationInputTokens: m.CacheWriteTokens,
		CacheReadInputTokens:     m.CachedReadTokens,
	}
}

// StreamEvent is any event in a Messages stream. ContentBlock and Delta stay
// raw because their shape depends on Type.
type StreamEvent struct {
	Type         string            `json:"type"`
	Message      *MessagesResponse `json:"message,omitempty"`
	Index        *int              `json:"index,omitempty"`
	ContentBlock json.RawMessage   `json:"content_block,omitempty"`
	Delta        json.RawMessage   `json:"delta,omitempty"`
	Usage        *Usage            `json:"usage,omitempty"`
	Error        *ErrorBody        `json:"error,omitempty"`
}

// BlockDelta is the delta of a content_block_delta event.
type BlockDelta struct {
	Type        string `json:"type"` // text_delta, input_json_delta, thinking_delta, signature_delta
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// MessageDelta is the delta of a message_delta event.
type MessageDelta struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type ErrorResponse struct {
	Type  string    `json:"type"` // always "error"
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// ParseContent decodes message or system content, which is either a string
// or an array of blocks. A string becomes a single text block.
func ParseContent(raw json.RawMessage) ([]ContentBlock, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	if s[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []ContentBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of blocks: %w", err)
	}
	return blocks, nil
}

// BlocksText joins the text of all text blocks with sep.
func BlocksText(blocks []ContentBlock, sep string) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, sep)
}
