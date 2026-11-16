package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/sse"
	"github.com/mp2209/pitwall/internal/translate"
)

// exchange is a call ready to send: the provider-format body plus how to
// turn the provider's answer back into the client's format.
type exchange struct {
	*call
	path     string // provider path, for example /v1/messages
	rawQuery string
	body     []byte
	// convert turns a successful non-streaming provider body into the
	// client's format and extracts usage.
	convert func(body []byte) ([]byte, metering.Usage, error)
	// stream handles a streaming response.
	streamer translate.Stream
}

const maxErrorBody = 1 << 20

func (s *Server) send(w http.ResponseWriter, r *http.Request, ex *exchange) {
	rec := ex.record()
	defer func() {
		rec.Latency = time.Since(ex.start)
		s.recorder.Record(context.WithoutCancel(r.Context()), rec)
	}()
	w.Header().Set("x-pitwall-request-id", ex.id)

	req, err := ex.route.Upstream.NewRequest(r.Context(), ex.path, ex.rawQuery, ex.body, r.Header)
	if err != nil {
		rec.Status, rec.Error = http.StatusInternalServerError, err.Error()
		writeError(w, ex.format, rec.Status, "gateway could not build the provider request")
		return
	}
	upstreamStart := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		rec.Overhead = time.Since(ex.start) - time.Since(upstreamStart)
		if r.Context().Err() != nil {
			rec.ClientGone, rec.Status, rec.Error = true, 499, "client disconnected before the provider answered"
			return
		}
		rec.Status, rec.Error = http.StatusBadGateway, err.Error()
		writeError(w, ex.format, rec.Status, fmt.Sprintf("provider %s unreachable", ex.route.Upstream.Name))
		return
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	finishOverhead := func() {
		rec.Overhead = time.Since(ex.start) - time.Since(upstreamStart)
	}

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		rec.Error = translate.ErrorMessage(body)
		finishOverhead()
		if ex.format == ex.route.Upstream.Kind {
			copyHeader(w.Header(), resp.Header, "Content-Type", "Retry-After", "request-id", "x-request-id")
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
			return
		}
		if v := resp.Header.Get("Retry-After"); v != "" {
			w.Header().Set("Retry-After", v)
		}
		writeError(w, ex.format, resp.StatusCode, rec.Error)
		return
	}

	if !ex.stream {
		body, err := io.ReadAll(resp.Body)
		upstreamDone := time.Now()
		if err != nil {
			rec.Status, rec.Error = http.StatusBadGateway, "reading provider response: "+err.Error()
			rec.ClientGone = r.Context().Err() != nil
			writeError(w, ex.format, rec.Status, rec.Error)
			return
		}
		out, usage, err := ex.convert(body)
		if err != nil {
			rec.Status, rec.Error = http.StatusBadGateway, "decoding provider response: "+err.Error()
			writeError(w, ex.format, rec.Status, rec.Error)
			return
		}
		if usage.IsZero() {
			usage = metering.Usage{
				InputTokens:  metering.EstimateTokens(ex.promptLen),
				OutputTokens: metering.EstimateTokens(len(body)),
				Estimated:    true,
			}
		}
		rec.Usage = usage
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		rec.Overhead = time.Since(ex.start) - upstreamDone.Sub(upstreamStart)
		return
	}

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		rec.Status, rec.Error = http.StatusBadGateway, "provider answered a stream request with "+ct
		writeError(w, ex.format, rec.Status, rec.Error)
		return
	}
	s.pipeStream(w, r, resp, ex, &rec)
	finishOverhead()
}

// pipeStream relays a provider stream to the client one event at a time,
// flushing after each, and records usage even if either side drops.
func (s *Server) pipeStream(w http.ResponseWriter, r *http.Request, resp *http.Response, ex *exchange, rec *Record) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	defer func() { rec.Usage = ex.streamer.Usage() }()
	reader := sse.NewReader(resp.Body)
	first := true
	for {
		ev, err := reader.Next()
		if errors.Is(err, io.EOF) {
			if err := ex.streamer.End(w); err == nil {
				_ = rc.Flush()
			}
			return
		}
		if err != nil {
			if r.Context().Err() != nil {
				rec.ClientGone, rec.Error = true, "client disconnected mid-stream"
				return
			}
			rec.Error = "provider stream broke: " + err.Error()
			writeStreamError(w, ex.format, rec.Error)
			_ = rc.Flush()
			return
		}
		if first && !ev.IsComment() {
			first = false
			rec.TTFT = time.Since(ex.start)
		}
		if err := ex.streamer.Event(ev, w); err != nil {
			if r.Context().Err() != nil {
				rec.ClientGone, rec.Error = true, "client disconnected mid-stream"
			} else {
				rec.Error = err.Error()
			}
			return
		}
		if err := rc.Flush(); err != nil {
			rec.ClientGone, rec.Error = true, "client disconnected mid-stream"
			return
		}
	}
}

func copyHeader(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Values(k); len(v) > 0 {
			dst[http.CanonicalHeaderKey(k)] = v
		}
	}
}

// patchBody sets fields on a raw JSON object while keeping every other
// field as the client sent it.
func patchBody(body []byte, set map[string]any) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	for k, v := range set {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		obj[k] = b
	}
	return json.Marshal(obj)
}
