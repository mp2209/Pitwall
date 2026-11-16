// Package translate converts requests, responses, streams and errors between
// the OpenAI Chat Completions and Anthropic Messages formats.
//
// Same-format calls never pass through here: the gateway forwards them as
// raw JSON so that fields this package does not model still reach the
// provider. Translation is only for cross-format calls, where some loss is
// unavoidable; the comments on each function say what is dropped.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

var emptyObject = json.RawMessage(`{}`)

// OpenAIToAnthropicRequest converts a chat completion request to a Messages
// request. System and developer messages become the system prompt; tool
// messages become tool_result blocks; consecutive same-role messages are
// merged because Anthropic requires roles to alternate. response_format,
// logprobs, n and seed have no Anthropic equivalent and are dropped.
func OpenAIToAnthropicRequest(in *openai.ChatRequest, model string, defaultMaxTokens int) (*anthropic.MessagesRequest, error) {
	out := &anthropic.MessagesRequest{
		Model:       model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	switch {
	case in.MaxCompletionTokens != nil:
		out.MaxTokens = *in.MaxCompletionTokens
	case in.MaxTokens != nil:
		out.MaxTokens = *in.MaxTokens
	default:
		out.MaxTokens = defaultMaxTokens
	}
	stops, err := parseStop(in.Stop)
	if err != nil {
		return nil, err
	}
	out.StopSequences = stops
	if in.User != "" {
		out.Metadata = &anthropic.Metadata{UserID: in.User}
	}

	var system []anthropic.ContentBlock
	var msgs messageBuilder
	for i, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := openai.ContentText(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			if text != "" {
				system = append(system, anthropic.ContentBlock{Type: "text", Text: text})
			}
		case "user":
			blocks, err := openAIUserBlocks(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			msgs.add("user", blocks...)
		case "assistant":
			blocks, err := openAIAssistantBlocks(m)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			msgs.add("assistant", blocks...)
		case "tool":
			text, err := openai.ContentText(m.Content)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			content, _ := json.Marshal(text)
			msgs.add("user", anthropic.ContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content})
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(system) > 0 {
		out.System, _ = json.Marshal(system)
	}
	out.Messages, err = msgs.build()
	if err != nil {
		return nil, err
	}

	for _, t := range in.Tools {
		if t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropic.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	out.ToolChoice, err = openAIToolChoice(in.ToolChoice)
	if err != nil {
		return nil, err
	}
	if in.ParallelToolCalls != nil && !*in.ParallelToolCalls && len(out.Tools) > 0 {
		if out.ToolChoice == nil {
			out.ToolChoice = &anthropic.ToolChoice{Type: "auto"}
		}
		if out.ToolChoice.Type != "none" {
			t := true
			out.ToolChoice.DisableParallelToolUse = &t
		}
	}
	return out, nil
}

func parseStop(raw json.RawMessage) ([]string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	if s[0] == '"' {
		var one string
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("stop must be a string or array of strings: %w", err)
	}
	return many, nil
}

func openAIUserBlocks(raw json.RawMessage) ([]anthropic.ContentBlock, error) {
	text, parts, err := openai.ParseContent(raw)
	if err != nil {
		return nil, err
	}
	if parts == nil {
		if text == "" {
			return nil, nil
		}
		return []anthropic.ContentBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []anthropic.ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			blocks = append(blocks, anthropic.ContentBlock{Type: "image", Source: imageSource(p.ImageURL.URL)})
		}
	}
	return blocks, nil
}

// imageSource turns an OpenAI image URL, which may be a data URL, into an
// Anthropic image source.
func imageSource(url string) *anthropic.ImageSource {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		if mediaType, isB64 := strings.CutSuffix(meta, ";base64"); found && isB64 {
			return &anthropic.ImageSource{Type: "base64", MediaType: mediaType, Data: data}
		}
	}
	return &anthropic.ImageSource{Type: "url", URL: url}
}

func openAIAssistantBlocks(m openai.Message) ([]anthropic.ContentBlock, error) {
	text, err := openai.ContentText(m.Content)
	if err != nil {
		return nil, err
	}
	var blocks []anthropic.ContentBlock
	if text != "" {
		blocks = append(blocks, anthropic.ContentBlock{Type: "text", Text: text})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, anthropic.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: toolInput(tc.Function.Arguments),
		})
	}
	return blocks, nil
}

// toolInput turns OpenAI's JSON-string arguments into an Anthropic input
// object. Arguments that are not a JSON object are kept under "arguments"
// rather than silently dropped.
func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return emptyObject
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err == nil && obj != nil {
		return json.RawMessage(args)
	}
	wrapped, _ := json.Marshal(map[string]string{"arguments": args})
	return wrapped
}

func openAIToolChoice(raw json.RawMessage) (*anthropic.ToolChoice, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	if s[0] == '"' {
		var choice string
		_ = json.Unmarshal(raw, &choice)
		switch choice {
		case "auto":
			return &anthropic.ToolChoice{Type: "auto"}, nil
		case "none":
			return &anthropic.ToolChoice{Type: "none"}, nil
		case "required":
			return &anthropic.ToolChoice{Type: "any"}, nil
		}
		return nil, fmt.Errorf("unsupported tool_choice %q", choice)
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Function.Name == "" {
		return nil, fmt.Errorf("unsupported tool_choice %s", s)
	}
	return &anthropic.ToolChoice{Type: "tool", Name: obj.Function.Name}, nil
}

// messageBuilder merges consecutive same-role turns, which Anthropic needs
// when several OpenAI tool messages answer one parallel tool call.
type messageBuilder struct {
	roles  []string
	blocks [][]anthropic.ContentBlock
}

func (b *messageBuilder) add(role string, blocks ...anthropic.ContentBlock) {
	if len(blocks) == 0 {
		return
	}
	if n := len(b.roles); n > 0 && b.roles[n-1] == role {
		b.blocks[n-1] = append(b.blocks[n-1], blocks...)
		return
	}
	b.roles = append(b.roles, role)
	b.blocks = append(b.blocks, blocks)
}

func (b *messageBuilder) build() ([]anthropic.Message, error) {
	out := make([]anthropic.Message, len(b.roles))
	for i, role := range b.roles {
		content, err := json.Marshal(b.blocks[i])
		if err != nil {
			return nil, err
		}
		out[i] = anthropic.Message{Role: role, Content: content}
	}
	return out, nil
}

// AnthropicToOpenAIRequest converts a Messages request to a chat completion
// request. tool_result blocks become tool messages placed before the rest of
// that user turn, as OpenAI requires them right after the assistant's tool
// calls. Thinking blocks, top_k, cache_control and Anthropic server tools
// (web search, code execution and other typed tools) are dropped.
func AnthropicToOpenAIRequest(in *anthropic.MessagesRequest, model, maxTokensParam string) (*openai.ChatRequest, error) {
	out := &openai.ChatRequest{
		Model:       model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stream:      in.Stream,
	}
	if in.MaxTokens > 0 {
		n := in.MaxTokens
		if maxTokensParam == "max_completion_tokens" {
			out.MaxCompletionTokens = &n
		} else {
			out.MaxTokens = &n
		}
	}
	if in.Stream {
		out.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	if len(in.StopSequences) > 0 {
		out.Stop, _ = json.Marshal(in.StopSequences)
	}
	if in.Metadata != nil {
		out.User = in.Metadata.UserID
	}

	system, err := anthropic.ParseContent(in.System)
	if err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	if text := anthropic.BlocksText(system, "\n\n"); text != "" {
		out.Messages = append(out.Messages, openai.Message{Role: "system", Content: jsonString(text)})
	}

	for i, m := range in.Messages {
		blocks, err := anthropic.ParseContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "user":
			msgs, err := anthropicUserMessages(blocks)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			out.Messages = append(out.Messages, msgs...)
		case "assistant":
			out.Messages = append(out.Messages, anthropicAssistantMessage(blocks))
		case "system":
			// Claude Code sends mid-conversation system turns; OpenAI
			// accepts system messages at any position.
			if text := anthropic.BlocksText(blocks, "\n\n"); text != "" {
				out.Messages = append(out.Messages, openai.Message{Role: "system", Content: jsonString(text)})
			}
		default:
			return nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}

	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "custom" {
			continue // server or built-in tool, no OpenAI equivalent
		}
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, openai.Tool{
			Type:     "function",
			Function: openai.FunctionDef{Name: t.Name, Description: t.Description, Parameters: schema},
		})
	}
	if tc := in.ToolChoice; tc != nil && len(out.Tools) > 0 {
		switch tc.Type {
		case "auto":
			out.ToolChoice = jsonString("auto")
		case "any":
			out.ToolChoice = jsonString("required")
		case "none":
			out.ToolChoice = jsonString("none")
		case "tool":
			out.ToolChoice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
		}
		if tc.DisableParallelToolUse != nil && *tc.DisableParallelToolUse {
			f := false
			out.ParallelToolCalls = &f
		}
	}
	return out, nil
}

func anthropicUserMessages(blocks []anthropic.ContentBlock) ([]openai.Message, error) {
	var out []openai.Message
	var parts []openai.ContentPart
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			text, err := toolResultText(b)
			if err != nil {
				return nil, err
			}
			out = append(out, openai.Message{Role: "tool", ToolCallID: b.ToolUseID, Content: jsonString(text)})
		case "text":
			parts = append(parts, openai.ContentPart{Type: "text", Text: b.Text})
		case "image":
			if b.Source == nil {
				continue
			}
			url := b.Source.URL
			if b.Source.Type == "base64" {
				url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
			}
			parts = append(parts, openai.ContentPart{Type: "image_url", ImageURL: &openai.ImageURL{URL: url}})
		}
	}
	switch {
	case len(parts) == 1 && parts[0].Type == "text":
		out = append(out, openai.Message{Role: "user", Content: jsonString(parts[0].Text)})
	case len(parts) > 0:
		content, _ := json.Marshal(parts)
		out = append(out, openai.Message{Role: "user", Content: content})
	}
	return out, nil
}

func toolResultText(b anthropic.ContentBlock) (string, error) {
	inner, err := anthropic.ParseContent(b.Content)
	if err != nil {
		return "", fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
	}
	var parts []string
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			parts = append(parts, ib.Text)
		case "image":
			parts = append(parts, "[image omitted]")
		}
	}
	text := strings.Join(parts, "\n")
	if b.IsError {
		text = "Error: " + text
	}
	return text, nil
}

func anthropicAssistantMessage(blocks []anthropic.ContentBlock) openai.Message {
	m := openai.Message{Role: "assistant"}
	if text := anthropic.BlocksText(blocks, "\n"); text != "" {
		m.Content = jsonString(text)
	} else {
		m.Content = json.RawMessage("null")
	}
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := string(b.Input)
		if args == "" {
			args = "{}"
		}
		m.ToolCalls = append(m.ToolCalls, openai.ToolCall{
			ID:       b.ID,
			Type:     "function",
			Function: openai.FunctionCall{Name: b.Name, Arguments: args},
		})
	}
	return m
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
