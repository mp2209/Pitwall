package tasks

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"

	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
)

// LoopStats describes how repetitive the end of a conversation is. It is
// read from the history every call resends, so it needs no stored state and
// works for inferred tasks as well as labeled ones.
type LoopStats struct {
	// RepeatedExchanges counts how many times in a row the newest exchange
	// (an assistant turn plus what came back) occurred: the same action with
	// the same result, which is what a stuck agent looks like.
	RepeatedExchanges int
	// RepeatedToolCalls counts how many times in a row the newest assistant
	// turn made the same tool calls with the same arguments, whatever the
	// results were.
	RepeatedToolCalls int
	// LastToolCall is the newest tool call, shortened, for messages.
	LastToolCall string
}

// loopWindow bounds how much history is examined per call.
const loopWindow = 40

// loopMsg is one message reduced for loop detection.
type loopMsg struct {
	assistant bool
	key       uint64 // content with IDs removed and digits normalized
	tools     uint64 // tool calls (name and canonical arguments); 0 if none
	toolDesc  string
}

// digits are normalized in compared text: test output carries timings and
// counters, and a run that fails the same way should look the same.
var digits = regexp.MustCompile(`\d+`)

func normText(s string) string { return digits.ReplaceAllString(strings.TrimSpace(s), "0") }

// normResult normalizes a tool result: digits as in normText, and lines
// sorted, because test runners often print the same failures in a different
// order each run (Go map iteration, parallel tests). Same lines in any order
// means nothing changed.
func normResult(s string) string {
	lines := strings.Split(normText(s), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func hashStrings(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// canonicalJSON re-serializes JSON with sorted keys, so argument order and
// spacing don't matter.
func canonicalJSON(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(bytes.TrimSpace(raw))
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func describe(name, args string) string {
	d := name + " " + args
	if len(d) > 120 {
		d = d[:117] + "..."
	}
	return d
}

func toolSignature(calls []string) uint64 {
	if len(calls) == 0 {
		return 0
	}
	sorted := append([]string(nil), calls...)
	sort.Strings(sorted) // parallel calls in any order are the same action
	return hashStrings(sorted...)
}

func anthropicLoopMsgs(msgs []anthropic.Message) []loopMsg {
	msgs = msgs[max(0, len(msgs)-loopWindow):]
	out := make([]loopMsg, 0, len(msgs))
	for _, m := range msgs {
		blocks, err := anthropic.ParseContent(m.Content)
		if err != nil {
			continue
		}
		var parts, calls []string
		desc := ""
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, "text:"+normText(b.Text))
			case "tool_use":
				sig := b.Name + " " + canonicalJSON(b.Input)
				calls = append(calls, sig)
				if desc == "" {
					desc = describe(b.Name, canonicalJSON(b.Input))
				}
			case "tool_result":
				inner, _ := anthropic.ParseContent(b.Content)
				text := anthropic.BlocksText(inner, "\n")
				parts = append(parts, "result:"+normResult(text))
				if b.IsError {
					parts = append(parts, "error")
				}
			case "thinking", "redacted_thinking":
				// Reasoning differs between attempts at the same action.
			default:
				parts = append(parts, b.Type)
			}
		}
		parts = append(parts, calls...)
		out = append(out, loopMsg{
			assistant: m.Role == "assistant",
			key:       hashStrings(append([]string{m.Role}, parts...)...),
			tools:     toolSignature(calls),
			toolDesc:  desc,
		})
	}
	return out
}

func openAILoopMsgs(msgs []openai.Message) []loopMsg {
	msgs = msgs[max(0, len(msgs)-loopWindow):]
	out := make([]loopMsg, 0, len(msgs))
	for _, m := range msgs {
		text, _ := openai.ContentText(m.Content)
		norm := normText(text)
		if m.Role == "tool" {
			norm = normResult(text)
		}
		parts := []string{m.Role, norm}
		var calls []string
		desc := ""
		for _, tc := range m.ToolCalls {
			args := canonicalJSON([]byte(tc.Function.Arguments))
			calls = append(calls, tc.Function.Name+" "+args)
			if desc == "" {
				desc = describe(tc.Function.Name, args)
			}
		}
		parts = append(parts, calls...)
		out = append(out, loopMsg{
			assistant: m.Role == "assistant",
			key:       hashStrings(parts...),
			tools:     toolSignature(calls),
			toolDesc:  desc,
		})
	}
	return out
}

// loopStats measures repetition at the end of a conversation.
func loopStats(msgs []loopMsg) LoopStats {
	var st LoopStats
	// Exchanges: an assistant turn plus the messages that answer it. Only
	// complete exchanges count; the newest one ends the request.
	var exchanges []uint64
	var assistants []loopMsg
	for i := 0; i < len(msgs); i++ {
		if !msgs[i].assistant {
			continue
		}
		assistants = append(assistants, msgs[i])
		parts := []string{}
		j := i + 1
		for ; j < len(msgs) && !msgs[j].assistant; j++ {
			parts = append(parts, hashHex(msgs[j].key))
		}
		if j > i+1 {
			exchanges = append(exchanges, hashStrings(append([]string{hashHex(msgs[i].key)}, parts...)...))
		}
	}
	if n := len(exchanges); n > 0 {
		st.RepeatedExchanges = 1
		for k := n - 2; k >= 0 && exchanges[k] == exchanges[n-1]; k-- {
			st.RepeatedExchanges++
		}
	}
	if n := len(assistants); n > 0 && assistants[n-1].tools != 0 {
		st.RepeatedToolCalls = 1
		st.LastToolCall = assistants[n-1].toolDesc
		for k := n - 2; k >= 0 && assistants[k].tools == assistants[n-1].tools; k-- {
			st.RepeatedToolCalls++
		}
	}
	return st
}

func hashHex(v uint64) string {
	const hex = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = hex[v&0xf]
		v >>= 4
	}
	return string(b[:])
}
