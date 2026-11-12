package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
	"github.com/mp2209/pitwall/internal/translate"
)

// handleChatCompletions serves OpenAI-format clients.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	const format = config.KindOpenAI
	body, ok := s.readBody(w, r, format)
	if !ok {
		return
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	c, ok := s.newCall(w, format, req.Model, req.Stream, len(body))
	if !ok {
		return
	}
	ex := &exchange{call: c}
	clientWantsUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage

	switch c.route.Upstream.Kind {
	case config.KindOpenAI:
		set := map[string]any{"model": c.route.Model}
		if req.Stream {
			// Always ask for the final usage chunk; the passthrough strips
			// it again if the client did not ask.
			set["stream_options"] = mergeStreamOptions(body)
			ex.streamer = translate.NewOpenAIPassthrough(len(body), !clientWantsUsage)
		}
		up, err := patchBody(body, set)
		if err != nil {
			writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		ex.path, ex.body = "/v1/chat/completions", up
		ex.convert = func(b []byte) ([]byte, metering.Usage, error) {
			var resp openai.ChatResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return nil, metering.Usage{}, err
			}
			return b, resp.Usage.Metering(), nil
		}

	case config.KindAnthropic:
		areq, err := translate.OpenAIToAnthropicRequest(&req, c.route.Model, s.cfg.DefaultMaxTokens)
		if err != nil {
			writeError(w, format, http.StatusBadRequest, err.Error())
			return
		}
		up, err := json.Marshal(areq)
		if err != nil {
			writeError(w, format, http.StatusInternalServerError, err.Error())
			return
		}
		ex.path, ex.body = "/v1/messages", up
		if req.Stream {
			ex.streamer = translate.NewAnthropicToOpenAIStream(req.Model, clientWantsUsage, len(body))
		}
		ex.convert = func(b []byte) ([]byte, metering.Usage, error) {
			var resp anthropic.MessagesResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return nil, metering.Usage{}, err
			}
			out, err := json.Marshal(translate.AnthropicToOpenAIResponse(&resp, req.Model))
			return out, resp.Usage.Metering(), err
		}
	}
	s.send(w, r, ex)
}

// handleMessages serves Anthropic-format clients.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	const format = config.KindAnthropic
	body, ok := s.readBody(w, r, format)
	if !ok {
		return
	}
	var req anthropic.MessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	c, ok := s.newCall(w, format, req.Model, req.Stream, len(body))
	if !ok {
		return
	}
	ex := &exchange{call: c}

	switch c.route.Upstream.Kind {
	case config.KindAnthropic:
		up, err := patchBody(body, map[string]any{"model": c.route.Model})
		if err != nil {
			writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		ex.path, ex.rawQuery, ex.body = "/v1/messages", r.URL.RawQuery, up
		if req.Stream {
			ex.streamer = translate.NewAnthropicPassthrough(len(body))
		}
		ex.convert = func(b []byte) ([]byte, metering.Usage, error) {
			var resp anthropic.MessagesResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return nil, metering.Usage{}, err
			}
			return b, resp.Usage.Metering(), nil
		}

	case config.KindOpenAI:
		oreq, err := translate.AnthropicToOpenAIRequest(&req, c.route.Model, c.route.Upstream.MaxTokensParam)
		if err != nil {
			writeError(w, format, http.StatusBadRequest, err.Error())
			return
		}
		up, err := json.Marshal(oreq)
		if err != nil {
			writeError(w, format, http.StatusInternalServerError, err.Error())
			return
		}
		ex.path, ex.body = "/v1/chat/completions", up
		if req.Stream {
			ex.streamer = translate.NewOpenAIToAnthropicStream(req.Model, len(body))
		}
		ex.convert = func(b []byte) ([]byte, metering.Usage, error) {
			var resp openai.ChatResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return nil, metering.Usage{}, err
			}
			out, err := json.Marshal(translate.OpenAIToAnthropicResponse(&resp, req.Model))
			return out, resp.Usage.Metering(), err
		}
	}
	s.send(w, r, ex)
}

// handleCountTokens serves Anthropic's token-counting endpoint, which
// Claude Code calls. Anthropic-format providers answer it; for others the
// gateway estimates.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	const format = config.KindAnthropic
	body, ok := s.readBody(w, r, format)
	if !ok {
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	route, found := s.router.Resolve(req.Model)
	if !found {
		writeError(w, format, http.StatusNotFound, fmt.Sprintf("model %q is not configured on this gateway", req.Model))
		return
	}
	if route.Upstream.Kind != config.KindAnthropic {
		writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": metering.EstimateTokens(len(body))})
		return
	}
	up, err := patchBody(body, map[string]any{"model": route.Model})
	if err != nil {
		writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req2, err := route.Upstream.NewRequest(r.Context(), "/v1/messages/count_tokens", r.URL.RawQuery, up, r.Header)
	if err != nil {
		writeError(w, format, http.StatusInternalServerError, err.Error())
		return
	}
	resp, err := s.client.Do(req2)
	if err != nil {
		writeError(w, format, http.StatusBadGateway, fmt.Sprintf("provider %s unreachable", route.Upstream.Name))
		return
	}
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header, "Content-Type")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// newCall resolves the model and starts the call's bookkeeping, writing an
// error and returning false if the model is unknown.
func (s *Server) newCall(w http.ResponseWriter, format, model string, stream bool, promptLen int) (*call, bool) {
	if model == "" {
		writeError(w, format, http.StatusBadRequest, "model is required")
		return nil, false
	}
	route, found := s.router.Resolve(model)
	if !found {
		writeError(w, format, http.StatusNotFound, fmt.Sprintf("model %q is not configured on this gateway", model))
		return nil, false
	}
	return &call{
		id:        newRequestID(),
		start:     time.Now(),
		format:    format,
		model:     model,
		route:     route,
		stream:    stream,
		promptLen: promptLen,
	}, true
}

// mergeStreamOptions keeps any stream_options the client sent and turns on
// include_usage.
func mergeStreamOptions(body []byte) map[string]any {
	var probe struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &probe)
	opts := probe.StreamOptions
	if opts == nil {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	return opts
}
