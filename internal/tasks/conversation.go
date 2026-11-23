// Package tasks groups model calls into agent tasks.
//
// A task is one agent run, from its first model call to its last. A call
// joins a task in one of two ways: explicitly, when the client sends an
// x-pitwall-task header, or by inference, for agents that can't be changed.
// Inference relies on how agents work: every call resends the conversation so
// far, so a call belongs to the open task whose previous call its message
// list extends, with the same system prompt and opening turn.
package tasks

import (
	"bytes"
	"encoding/json"
	"hash/fnv"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

// Conversation is a request reduced to what task tracking needs. It is made
// of pieces in the order providers read them: tools, system prompt, then one
// piece per message. Prefix[i] hashes pieces 0..i, and Bytes[i] is their
// size, so two calls share their first k pieces exactly when their first k
// prefix hashes match.
type Conversation struct {
	Opening  uint64 // system prompt and first user message
	Prefix   []uint64
	Bytes    []int
	Messages int // conversation messages, not counting tools and system
	// Loop measures repetition at the end of the conversation, for the
	// runaway-agent guard.
	Loop LoopStats

	pieces [][]byte
}

// headerPieces is the number of pieces before the first message.
const headerPieces = 2

// TotalBytes is the size of the whole prompt.
func (c Conversation) TotalBytes() int {
	if len(c.Bytes) == 0 {
		return 0
	}
	return c.Bytes[len(c.Bytes)-1]
}

// Message is one conversation turn as the client sent it.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// NewMessages returns the messages after the first shared pieces: what a
// call added to the conversation since the previous call of its task.
func (c Conversation) NewMessages(shared int) []Message {
	start := max(shared, headerPieces)
	if start >= len(c.pieces) {
		return nil
	}
	out := make([]Message, 0, len(c.pieces)-start)
	for _, p := range c.pieces[start:] {
		role, content, _ := bytes.Cut(p, []byte{0})
		out = append(out, Message{Role: string(role), Content: content})
	}
	return out
}

type builder struct {
	conv Conversation
	h    interface {
		Write([]byte) (int, error)
		Sum64() uint64
	}
	total int
}

func newBuilder() *builder { return &builder{h: fnv.New64a()} }

func (b *builder) add(piece []byte) {
	_, _ = b.h.Write(piece)
	_, _ = b.h.Write([]byte{0xff}) // separator, so piece boundaries count
	b.total += len(piece)
	b.conv.Prefix = append(b.conv.Prefix, b.h.Sum64())
	b.conv.Bytes = append(b.conv.Bytes, b.total)
	b.conv.pieces = append(b.conv.pieces, piece)
}

func opening(system, firstUser []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(system)
	_, _ = h.Write([]byte{0xff})
	_, _ = h.Write(firstUser)
	return h.Sum64()
}

// stripCacheControl removes cache_control markers, which agents (and, in
// Phase 4, the gateway) move to the newest turn on every call; leaving them
// in would make an unchanged message look edited.
func stripCacheControl(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`"cache_control"`)) {
		return bytes.TrimSpace(raw)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return bytes.TrimSpace(raw)
	}
	out, err := json.Marshal(strip(v))
	if err != nil {
		return bytes.TrimSpace(raw)
	}
	return out
}

func strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for k, x := range t {
			t[k] = strip(x)
		}
	case []any:
		for i, x := range t {
			t[i] = strip(x)
		}
	}
	return v
}

func message(role string, content []byte) []byte {
	return append(append([]byte(role), 0), content...)
}

// FromAnthropic reduces a Messages request.
func FromAnthropic(req *anthropic.MessagesRequest) Conversation {
	b := newBuilder()
	tools := make([]anthropic.Tool, len(req.Tools))
	for i, t := range req.Tools {
		t.CacheControl = nil
		tools[i] = t
	}
	toolBytes, _ := json.Marshal(tools)
	b.add(toolBytes)
	system := stripCacheControl(req.System)
	b.add(system)
	var firstUser []byte
	for _, m := range req.Messages {
		piece := message(m.Role, stripCacheControl(m.Content))
		b.add(piece)
		if firstUser == nil && m.Role == "user" {
			firstUser = piece
		}
	}
	b.conv.Messages = len(req.Messages)
	b.conv.Opening = opening(system, firstUser)
	b.conv.Loop = loopStats(anthropicLoopMsgs(req.Messages))
	return b.conv
}

// FromOpenAI reduces a chat completion request. Leading system and
// developer messages are the system prompt; later ones count as turns.
func FromOpenAI(req *openai.ChatRequest) Conversation {
	b := newBuilder()
	toolBytes, _ := json.Marshal(req.Tools)
	b.add(toolBytes)
	i := 0
	var system []byte
	for ; i < len(req.Messages); i++ {
		m := req.Messages[i]
		if m.Role != "system" && m.Role != "developer" {
			break
		}
		system = append(system, message(m.Role, bytes.TrimSpace(m.Content))...)
	}
	b.add(system)
	var firstUser []byte
	for _, m := range req.Messages[i:] {
		raw, _ := json.Marshal(m)
		piece := message(m.Role, raw)
		b.add(piece)
		if firstUser == nil && m.Role == "user" {
			firstUser = piece
		}
	}
	b.conv.Messages = len(req.Messages) - i
	b.conv.Opening = opening(system, firstUser)
	b.conv.Loop = loopStats(openAILoopMsgs(req.Messages[i:]))
	return b.conv
}

// sharedPrefix counts the leading pieces two calls have in common.
func sharedPrefix(a, b []uint64) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// extends reports whether cur starts with every piece of prev.
func extends(cur, prev []uint64) bool {
	return len(prev) > 0 && len(cur) >= len(prev) && cur[len(prev)-1] == prev[len(prev)-1]
}
