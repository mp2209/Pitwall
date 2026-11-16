// Package sse reads and writes server-sent event streams.
//
// The reader keeps each event's raw bytes so the gateway can forward an
// upstream stream byte-for-byte while still inspecting the parsed fields.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// Event is one server-sent event.
type Event struct {
	Event string // value of the "event:" field, empty if absent
	Data  []byte // "data:" lines joined with "\n"
	Raw   []byte // the event exactly as received, including the blank line
}

// IsComment reports whether the event carried no fields, only comments
// (for example ": keep-alive").
func (e *Event) IsComment() bool { return e.Event == "" && e.Data == nil }

// Reader parses an event stream.
type Reader struct {
	r *bufio.Reader
}

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event, or io.EOF once the stream ends.
func (r *Reader) Next() (*Event, error) {
	var (
		ev   Event
		raw  bytes.Buffer
		data [][]byte
		seen bool
	)
	finish := func() *Event {
		if data != nil {
			ev.Data = bytes.Join(data, []byte("\n"))
		}
		ev.Raw = raw.Bytes()
		return &ev
	}
	for {
		line, err := r.r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 && len(line) > 0 {
			// Blank line: dispatch, unless nothing has been read yet.
			if !seen {
				if err != nil {
					return nil, io.EOF
				}
				continue
			}
			raw.Write(line)
			return finish(), nil
		}
		if len(line) > 0 {
			raw.Write(line)
			seen = true
			parseField(trimmed, &ev, &data)
		}
		if err != nil { // EOF without a trailing blank line
			if seen {
				return finish(), nil
			}
			return nil, io.EOF
		}
	}
}

func parseField(line []byte, ev *Event, data *[][]byte) {
	if line[0] == ':' {
		return // comment
	}
	name, value, found := bytes.Cut(line, []byte(":"))
	if found && len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(name) {
	case "data":
		*data = append(*data, append([]byte(nil), value...))
	case "event":
		ev.Event = string(value)
	}
}

// Write writes one event. An empty name writes a data-only event, which is
// what OpenAI-format streams use.
func Write(w io.Writer, name string, data []byte) error {
	var buf bytes.Buffer
	if name != "" {
		fmt.Fprintf(&buf, "event: %s\n", name)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		buf.WriteString("data: ")
		buf.Write(line)
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}
