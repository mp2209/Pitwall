// Package mockprovider is a fake model provider that speaks both the OpenAI
// Chat Completions and Anthropic Messages formats. Tests, load tests and
// local development use it instead of paying for real tokens.
//
// Behavior is deterministic and can be steered three ways, later ones
// winning: the Config defaults, request headers (X-Mock-Mode,
// X-Mock-Delay-Ms, X-Mock-Chunk-Delay-Ms, X-Mock-Error), and directives in
// the prompt text such as [[mock:mode=loop]] or [[mock:error=529]]. Prompt
// directives exist because a gateway in between does not forward the
// client's headers.
//
// Modes:
//
//	auto  call the first tool if tools are offered and the last turn is not
//	      a tool result; otherwise answer with text (the default)
//	text  always answer with text
//	tool  always call the first tool
//	loop  always call the first tool with identical arguments, to seed
//	      runaway-agent tests
//
// [[mock:turns=N]] makes auto mode act like a multi-step agent: it calls the
// offered tools in turn (first, second, ... wrapping around) until the
// conversation has N assistant turns, then answers with text.
//
// [[mock:script=B]] makes auto mode play a fixed list of tool calls, one per
// assistant turn, then answer with text. B is base64url (no padding) of a
// JSON array like [{"name":"read_file","input":{"path":"a.go"}}]. The
// benchmark's dry run uses it to replay a reference fix through a real agent.
//
// Usage is counted at four bytes per token. Prompt caching is simulated: an
// Anthropic prefix ending at a cache_control mark, or an OpenAI prefix of at
// least MinCacheTokens ending at a message boundary, is a cache hit if an
// earlier request had the same prefix.
package mockprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config sets the mock's defaults.
type Config struct {
	Delay          time.Duration // before the first byte
	ChunkDelay     time.Duration // between stream chunks
	ErrorRate      float64       // share of requests that fail with 500
	MinCacheTokens int64         // smallest cacheable prefix, default 1024
}

// Server is the mock provider.
type Server struct {
	cfg   Config
	cache *prefixCache
	seq   atomic.Int64
	mu    sync.Mutex
	rng   *rand.Rand
}

func New(cfg Config) *Server {
	if cfg.MinCacheTokens == 0 {
		cfg.MinCacheTokens = 1024
	}
	return &Server{cfg: cfg, cache: newPrefixCache(), rng: rand.New(rand.NewPCG(1, 2))}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.handleOpenAI)
	mux.HandleFunc("POST /v1/messages", s.handleAnthropic)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "mock-model", "object": "model", "owned_by": "mock"},
		}})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

// behavior is the resolved plan for one request.
type behavior struct {
	mode       string
	turns      int
	delay      time.Duration
	chunkDelay time.Duration
	errStatus  int
	script     []scriptStep
}

// scriptStep is one tool call of a [[mock:script=...]] directive.
type scriptStep struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func parseScript(val string) ([]scriptStep, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(val)
	if err != nil {
		return nil, false
	}
	var steps []scriptStep
	if json.Unmarshal(raw, &steps) != nil {
		return nil, false
	}
	for i, st := range steps {
		if st.Name == "" {
			return nil, false
		}
		if len(st.Input) == 0 || string(st.Input) == "null" {
			steps[i].Input = json.RawMessage(`{}`)
		}
	}
	return steps, true
}

var directiveRe = regexp.MustCompile(`\[\[mock:([a-z_]+)=([^\]]+)\]\]`)

func (s *Server) behavior(r *http.Request, promptText string) behavior {
	b := behavior{mode: "auto", delay: s.cfg.Delay, chunkDelay: s.cfg.ChunkDelay}
	if s.cfg.ErrorRate > 0 {
		s.mu.Lock()
		fail := s.rng.Float64() < s.cfg.ErrorRate
		s.mu.Unlock()
		if fail {
			b.errStatus = http.StatusInternalServerError
		}
	}
	set := func(key, val string) {
		switch key {
		case "mode":
			b.mode = val
		case "delay", "delay_ms":
			if ms, err := strconv.Atoi(val); err == nil {
				b.delay = time.Duration(ms) * time.Millisecond
			}
		case "chunk_delay", "chunk_delay_ms":
			if ms, err := strconv.Atoi(val); err == nil {
				b.chunkDelay = time.Duration(ms) * time.Millisecond
			}
		case "error":
			if code, err := strconv.Atoi(val); err == nil {
				b.errStatus = code
			}
		case "turns":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				b.turns = n
			}
		case "script":
			if steps, ok := parseScript(val); ok {
				b.script = steps
			}
		}
	}
	for header, key := range map[string]string{
		"X-Mock-Mode": "mode", "X-Mock-Delay-Ms": "delay", "X-Mock-Chunk-Delay-Ms": "chunk_delay", "X-Mock-Error": "error",
	} {
		if v := r.Header.Get(header); v != "" {
			set(key, v)
		}
	}
	for _, m := range directiveRe.FindAllStringSubmatch(promptText, -1) {
		set(m[1], strings.TrimSpace(m[2]))
	}
	return b
}

// reply is what the mock decided to answer, independent of format.
type reply struct {
	text     string
	toolName string
	toolArgs string // JSON object
	toolID   string
}

func (r reply) isTool() bool { return r.toolName != "" }

func decide(b behavior, tools []toolDef, lastIsToolResult bool, lastUser string, assistantTurns int, n int64) reply {
	if b.script != nil && b.mode == "auto" {
		if assistantTurns < len(b.script) {
			st := b.script[assistantTurns]
			var compact bytes.Buffer
			if json.Compact(&compact, st.Input) != nil {
				compact.WriteString("{}")
			}
			return reply{toolName: st.Name, toolArgs: compact.String(), toolID: fmt.Sprintf("call_mock_%d", n)}
		}
		return reply{text: "The mock provider followed its script and is done."}
	}
	callTool, pick := false, 0
	switch b.mode {
	case "tool", "loop":
		callTool = len(tools) > 0
	case "text":
	default:
		if b.turns > 0 {
			callTool = len(tools) > 0 && assistantTurns < b.turns
			if len(tools) > 0 {
				pick = assistantTurns % len(tools)
			}
		} else {
			callTool = len(tools) > 0 && !lastIsToolResult
		}
	}
	if callTool {
		t := tools[pick]
		return reply{
			toolName: t.name,
			toolArgs: argsFromSchema(t.schema),
			toolID:   fmt.Sprintf("call_mock_%d", n),
		}
	}
	if lastIsToolResult {
		return reply{text: "The mock provider read the tool result and is done."}
	}
	snippet := strings.Join(strings.Fields(directiveRe.ReplaceAllString(lastUser, "")), " ")
	if len(snippet) > 60 {
		snippet = snippet[:60]
	}
	return reply{text: fmt.Sprintf("This is a mock reply to: %q. It streams a few words so clients see several chunks.", snippet)}
}

type toolDef struct {
	name   string
	schema json.RawMessage
}

// argsFromSchema fills each property of a JSON schema with a placeholder of
// the right type, so the arguments are valid for the tool.
func argsFromSchema(schema json.RawMessage) string {
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(schema, &s)
	args := map[string]any{}
	for name, p := range s.Properties {
		typ, _ := p.Type.(string)
		switch typ {
		case "integer", "number":
			args[name] = 1
		case "boolean":
			args[name] = true
		case "array":
			args[name] = []any{}
		case "object":
			args[name] = map[string]any{}
		default:
			args[name] = "mock"
		}
	}
	b, _ := json.Marshal(args) // map keys are sorted, so this is deterministic
	return string(b)
}

func tokens(n int) int64 { return int64((n + 3) / 4) }

// words splits text into stream chunks that rejoin to the original.
func words(text string) []string {
	var out []string
	for len(text) > 0 {
		i := strings.IndexByte(text[1:], ' ')
		if i < 0 {
			out = append(out, text)
			break
		}
		out = append(out, text[:i+1])
		text = text[i+1:]
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startStream(w http.ResponseWriter) *flushWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	_ = fw.rc.Flush()
	return fw
}

func (f *flushWriter) event(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if name != "" {
		if _, err := fmt.Fprintf(f.w, "event: %s\n", name); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(f.w, "data: %s\n\n", b); err != nil {
		return err
	}
	return f.rc.Flush()
}
