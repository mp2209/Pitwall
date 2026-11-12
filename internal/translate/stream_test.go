package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/sse"
)

// run feeds an upstream stream through s and returns the client stream.
func run(t *testing.T, s Stream, upstream string) []*sse.Event {
	t.Helper()
	var out bytes.Buffer
	r := sse.NewReader(strings.NewReader(upstream))
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Event(ev, &out); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.End(&out); err != nil {
		t.Fatal(err)
	}
	var evs []*sse.Event
	cr := sse.NewReader(&out)
	for {
		ev, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return evs
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
}

func anthropicEvents(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var probe struct{ Type string }
		_ = json.Unmarshal([]byte(e), &probe)
		b.WriteString("event: " + probe.Type + "\ndata: " + e + "\n\n")
	}
	return b.String()
}

var anthropicToolStream = anthropicEvents(
	`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"cache_read_input_tokens":90,"cache_creation_input_tokens":5,"output_tokens":1}}}`,
	`{"type":"ping"}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Running "}}`,
	`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"tests."}}`,
	`{"type":"content_block_stop","index":1}`,
	`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_a","name":"bash","input":{}}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`,
	`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"go test\"}"}}`,
	`{"type":"content_block_stop","index":2}`,
	`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_b","name":"ls","input":{}}}`,
	`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
	`{"type":"content_block_stop","index":3}`,
	`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":42}}`,
	`{"type":"message_stop"}`,
)

func TestAnthropicToOpenAIStream(t *testing.T) {
	s := NewAnthropicToOpenAIStream("my-alias", true, 100)
	evs := run(t, s, anthropicToolStream)

	var text strings.Builder
	args := map[int]string{}
	ids := map[int]string{}
	var finish string
	var usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	sawDone := false
	for i, ev := range evs {
		if ev.Event != "" {
			t.Errorf("OpenAI streams have no event names, got %q", ev.Event)
		}
		if string(ev.Data) == "[DONE]" {
			if i != len(evs)-1 {
				t.Error("[DONE] must be last")
			}
			sawDone = true
			continue
		}
		var c struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &c); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if c.ID != "chatcmpl-1" || c.Model != "my-alias" {
			t.Errorf("chunk %d id/model = %s/%s", i, c.ID, c.Model)
		}
		if len(c.Usage) > 0 {
			if len(c.Choices) != 0 {
				t.Error("usage chunk must have empty choices")
			}
			_ = json.Unmarshal(c.Usage, &usage)
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				text.WriteString(*ch.Delta.Content)
			}
			for _, tc := range ch.Delta.ToolCalls {
				args[tc.Index] += tc.Function.Arguments
				if tc.ID != "" {
					ids[tc.Index] = tc.ID + ":" + tc.Function.Name
				}
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if !sawDone {
		t.Error("missing [DONE]")
	}
	if text.String() != "Running tests." {
		t.Errorf("text = %q (thinking must be dropped)", text.String())
	}
	if ids[0] != "toolu_a:bash" || ids[1] != "toolu_b:ls" {
		t.Errorf("tool ids = %v", ids)
	}
	if args[0] != `{"cmd":"go test"}` || args[1] != `{}` {
		t.Errorf("tool args = %v", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q", finish)
	}
	if usage == nil || usage.PromptTokens != 105 || usage.CompletionTokens != 42 || usage.PromptTokensDetails.CachedTokens != 90 {
		t.Errorf("usage chunk = %+v", usage)
	}
	want := metering.Usage{InputTokens: 10, CachedReadTokens: 90, CacheWriteTokens: 5, OutputTokens: 42}
	if got := s.Usage(); got != want {
		t.Errorf("metered usage = %+v, want %+v", got, want)
	}
}

func TestAnthropicToOpenAIStreamWithoutUsageChunk(t *testing.T) {
	evs := run(t, NewAnthropicToOpenAIStream("m", false, 0), anthropicToolStream)
	for _, ev := range evs {
		if bytes.Contains(ev.Data, []byte(`"usage"`)) {
			t.Fatalf("client did not ask for usage but got %s", ev.Data)
		}
	}
}

func openAIChunks(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	return b.String()
}

var openAIToolStream = openAIChunks(
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"content":"Let me "},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"content":"check."},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	`{"id":"chatcmpl-9","object":"chat.completion.chunk","model":"gpt","choices":[],"usage":{"prompt_tokens":200,"completion_tokens":30,"total_tokens":230,"prompt_tokens_details":{"cached_tokens":128}}}`,
	`[DONE]`,
)

func TestOpenAIToAnthropicStream(t *testing.T) {
	s := NewOpenAIToAnthropicStream("my-alias", 100)
	evs := run(t, s, openAIToolStream)

	var types []string
	open := map[int]bool{}
	text := ""
	tools := map[int]string{}
	var stop string
	var finalUsage map[string]int64
	for _, ev := range evs {
		var e struct {
			Type         string          `json:"type"`
			Index        *int            `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
			Usage map[string]int64 `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			t.Fatal(err)
		}
		if ev.Event != e.Type {
			t.Errorf("event name %q != data type %q", ev.Event, e.Type)
		}
		types = append(types, e.Type)
		switch e.Type {
		case "message_start":
			if e.Message.ID != "msg_9" || e.Message.Model != "my-alias" {
				t.Errorf("message_start = %s", ev.Data)
			}
		case "content_block_start":
			for i, o := range open {
				if o {
					t.Errorf("block %d still open when %d starts", i, *e.Index)
				}
			}
			open[*e.Index] = true
			var b struct{ Type, ID, Name string }
			_ = json.Unmarshal(e.ContentBlock, &b)
			if b.Type == "tool_use" {
				tools[*e.Index] = b.ID + ":" + b.Name + ":"
			}
		case "content_block_delta":
			if !open[*e.Index] {
				t.Errorf("delta for closed block %d", *e.Index)
			}
			text += e.Delta.Text
			if e.Delta.Type == "input_json_delta" {
				tools[*e.Index] += e.Delta.PartialJSON
			}
		case "content_block_stop":
			open[*e.Index] = false
		case "message_delta":
			stop = e.Delta.StopReason
			finalUsage = e.Usage
		}
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" || types[len(types)-2] != "message_delta" {
		t.Errorf("event order = %v", types)
	}
	if text != "Let me check." {
		t.Errorf("text = %q", text)
	}
	if tools[1] != `call_a:bash:{"cmd":"ls"}` || tools[2] != `call_b:read:{}` {
		t.Errorf("tools = %v", tools)
	}
	if stop != "tool_use" {
		t.Errorf("stop = %q", stop)
	}
	if finalUsage["input_tokens"] != 72 || finalUsage["cache_read_input_tokens"] != 128 || finalUsage["output_tokens"] != 30 {
		t.Errorf("final usage = %v", finalUsage)
	}
}

func TestOpenAIToAnthropicStreamEstimatesWithoutUsage(t *testing.T) {
	// Ollama-style: no usage chunk, tool call without an index, no [DONE].
	upstream := openAIChunks(
		`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"hello world!"}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_z","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	s := NewOpenAIToAnthropicStream("m", 400)
	evs := run(t, s, upstream)
	if last := evs[len(evs)-1]; last.Event != "message_stop" {
		t.Fatalf("stream not closed: last event %q", last.Event)
	}
	u := s.Usage()
	if !u.Estimated || u.InputTokens != 100 || u.OutputTokens == 0 {
		t.Errorf("usage = %+v, want estimated with 100 input tokens", u)
	}
}

func TestOpenAIPassthroughStripsForcedUsage(t *testing.T) {
	s := NewOpenAIPassthrough(10, true)
	evs := run(t, s, openAIToolStream)
	for _, ev := range evs {
		if bytes.Contains(ev.Data, []byte(`"usage":{`)) {
			t.Fatal("usage chunk should be stripped")
		}
	}
	if string(evs[len(evs)-1].Data) != "[DONE]" {
		t.Error("[DONE] should pass through")
	}
	want := metering.Usage{InputTokens: 72, CachedReadTokens: 128, OutputTokens: 30}
	if got := s.Usage(); got != want {
		t.Errorf("usage = %+v want %+v", got, want)
	}

	kept := run(t, NewOpenAIPassthrough(10, false), openAIToolStream)
	if len(kept) != len(evs)+1 {
		t.Errorf("without stripping, all %d events should pass, got %d", len(evs)+1, len(kept))
	}
}

func TestAnthropicPassthroughIsByteExact(t *testing.T) {
	s := NewAnthropicPassthrough(10)
	var out bytes.Buffer
	r := sse.NewReader(strings.NewReader(anthropicToolStream))
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err := s.Event(ev, &out); err != nil {
			t.Fatal(err)
		}
	}
	if out.String() != anthropicToolStream {
		t.Error("passthrough changed the stream")
	}
	want := metering.Usage{InputTokens: 10, CachedReadTokens: 90, CacheWriteTokens: 5, OutputTokens: 42}
	if got := s.Usage(); got != want {
		t.Errorf("usage = %+v want %+v", got, want)
	}
}

func TestAnthropicPassthroughCutShortEstimatesOutput(t *testing.T) {
	s := NewAnthropicPassthrough(10)
	end := strings.Index(anthropicToolStream, "event: message_delta")
	if end < 0 {
		t.Fatal("fixture has no message_delta")
	}
	cut := anthropicToolStream[:end]
	run(t, s, cut)
	u := s.Usage()
	if !u.Estimated || u.InputTokens != 10 || u.CachedReadTokens != 90 || u.OutputTokens < 5 {
		t.Errorf("usage = %+v: input should be exact, output estimated", u)
	}
}
