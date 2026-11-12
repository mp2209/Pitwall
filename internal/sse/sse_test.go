package sse

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, s string) []*Event {
	t.Helper()
	r := NewReader(strings.NewReader(s))
	var out []*Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func TestReaderParsesFieldsAndKeepsRaw(t *testing.T) {
	in := "event: message_start\ndata: {\"a\":1}\n\n: ping\n\ndata: line1\ndata: line2\r\n\r\ndata: [DONE]"
	evs := readAll(t, in)
	if len(evs) != 4 {
		t.Fatalf("got %d events, want 4", len(evs))
	}
	if evs[0].Event != "message_start" || string(evs[0].Data) != `{"a":1}` {
		t.Errorf("event 0 = %+v", evs[0])
	}
	if string(evs[0].Raw) != "event: message_start\ndata: {\"a\":1}\n\n" {
		t.Errorf("raw 0 = %q", evs[0].Raw)
	}
	if !evs[1].IsComment() {
		t.Errorf("event 1 should be a comment: %+v", evs[1])
	}
	if string(evs[2].Data) != "line1\nline2" {
		t.Errorf("multi-line data = %q", evs[2].Data)
	}
	if string(evs[3].Data) != "[DONE]" {
		t.Errorf("unterminated final event = %q", evs[3].Data)
	}
	var joined bytes.Buffer
	for _, ev := range evs {
		joined.Write(ev.Raw)
	}
	if joined.String() != in {
		t.Errorf("raw bytes do not round-trip:\n got %q\nwant %q", joined.String(), in)
	}
}

func TestReaderSkipsLeadingBlankLines(t *testing.T) {
	evs := readAll(t, "\n\ndata: x\n\n\n")
	if len(evs) != 1 || string(evs[0].Data) != "x" {
		t.Fatalf("got %+v", evs)
	}
}

func TestWrite(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, "ping", []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, "", []byte("[DONE]")); err != nil {
		t.Fatal(err)
	}
	want := "event: ping\ndata: {\"type\":\"ping\"}\n\ndata: [DONE]\n\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
}
