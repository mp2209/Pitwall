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
	"github.com/mp2209/pitwall/internal/tasks"
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
	maxOutput := int64(s.cfg.DefaultMaxTokens)
	switch {
	case req.MaxCompletionTokens != nil:
		maxOutput = int64(*req.MaxCompletionTokens)
	case req.MaxTokens != nil:
		maxOutput = int64(*req.MaxTokens)
	}
	c, ok := s.newCall(w, r, callSpec{format: format, model: req.Model, stream: req.Stream, body: body, maxOutput: maxOutput,
		conv: tasks.FromOpenAI(&req)})
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
			ex.streamer = translate.NewOpenAIPassthrough(body, !clientWantsUsage)
		}
		up, err := patchBody(body, set)
		if err != nil {
			writeError(w, format, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		ex.path, ex.body = "/v1/chat/completions", up
		ex.convert = func(b []byte) (converted, error) {
			var resp openai.ChatResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return converted{}, err
			}
			return converted{body: b, usage: resp.Usage.Metering(), text: translate.OpenAIOutputText(&resp)}, nil
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
			ex.streamer = translate.NewAnthropicToOpenAIStream(req.Model, clientWantsUsage, body)
		}
		ex.convert = func(b []byte) (converted, error) {
			var resp anthropic.MessagesResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return converted{}, err
			}
			out, err := json.Marshal(translate.AnthropicToOpenAIResponse(&resp, req.Model))
			return converted{body: out, usage: resp.Usage.Metering(), text: translate.AnthropicOutputText(&resp)}, err
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
	maxOutput := int64(req.MaxTokens)
	if maxOutput <= 0 {
		maxOutput = int64(s.cfg.DefaultMaxTokens)
	}
	c, ok := s.newCall(w, r, callSpec{format: format, model: req.Model, stream: req.Stream, body: body, maxOutput: maxOutput,
		speed: req.Speed, conv: tasks.FromAnthropic(&req)})
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
			ex.streamer = translate.NewAnthropicPassthrough(body)
		}
		ex.convert = func(b []byte) (converted, error) {
			var resp anthropic.MessagesResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return converted{}, err
			}
			return converted{body: b, usage: resp.Usage.Metering(), text: translate.AnthropicOutputText(&resp)}, nil
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
			ex.streamer = translate.NewOpenAIToAnthropicStream(req.Model, body)
		}
		ex.convert = func(b []byte) (converted, error) {
			var resp openai.ChatResponse
			if err := json.Unmarshal(b, &resp); err != nil {
				return converted{}, err
			}
			out, err := json.Marshal(translate.OpenAIToAnthropicResponse(&resp, req.Model))
			return converted{body: out, usage: resp.Usage.Metering(), text: translate.OpenAIOutputText(&resp)}, err
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
	if key := keyFrom(r.Context()); key != nil && !key.AllowsModel(req.Model) {
		writeError(w, format, http.StatusForbidden, fmt.Sprintf("Pitwall key %s may not use model %q", key.Prefix, req.Model))
		return
	}
	route, found := s.router.Resolve(req.Model)
	if !found {
		writeError(w, format, http.StatusNotFound, fmt.Sprintf("model %q is not configured on this gateway", req.Model))
		return
	}
	if route.Upstream.Kind != config.KindAnthropic {
		writeJSON(w, http.StatusOK, map[string]int64{"input_tokens": metering.CountTokens(string(body))})
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

// callSpec is what a handler learned from the request body.
type callSpec struct {
	format    string
	model     string
	stream    bool
	body      []byte
	maxOutput int64
	speed     string
	conv      tasks.Conversation
}

// newCall resolves the model, checks the key may use it and starts the
// call's bookkeeping, writing an error and returning false if it can't go on.
func (s *Server) newCall(w http.ResponseWriter, r *http.Request, spec callSpec) (*call, bool) {
	if spec.model == "" {
		writeError(w, spec.format, http.StatusBadRequest, "model is required")
		return nil, false
	}
	key := keyFrom(r.Context())
	if key != nil && !key.AllowsModel(spec.model) {
		writeError(w, spec.format, http.StatusForbidden,
			fmt.Sprintf("Pitwall key %s may not use model %q", key.Prefix, spec.model))
		return nil, false
	}
	route, found := s.router.Resolve(spec.model)
	if !found {
		writeError(w, spec.format, http.StatusNotFound, fmt.Sprintf("model %q is not configured on this gateway", spec.model))
		return nil, false
	}
	speed := ""
	if spec.speed == "fast" {
		speed = "fast"
	}
	return &call{
		id:        newRequestID(),
		start:     time.Now(),
		format:    spec.format,
		model:     spec.model,
		route:     route,
		stream:    spec.stream,
		prompt:    spec.body,
		maxOutput: spec.maxOutput,
		speed:     speed,
		key:       key,

		conv:        spec.conv,
		taskHeader:  r.Header.Get(tasks.HeaderTask),
		agent:       tasks.AgentName(r.Header.Get(tasks.HeaderAgent), r.Header.Get("User-Agent")),
		configLabel: tasks.CleanLabel(r.Header.Get(tasks.HeaderConfig)),
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
