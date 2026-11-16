package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/internal/providers/anthropic"
	"github.com/mp2209/pitwall/internal/providers/openai"
	"github.com/mp2209/pitwall/internal/sse"
	"github.com/mp2209/pitwall/internal/translate"
)

// writeError writes an error in the format the client speaks.
func writeError(w http.ResponseWriter, format string, status int, msg string) {
	var body any
	if format == config.KindAnthropic {
		body = anthropic.ErrorResponse{
			Type:  "error",
			Error: anthropic.ErrorBody{Type: translate.AnthropicErrorType(status), Message: msg},
		}
	} else {
		body = openai.ErrorResponse{Error: openai.ErrorBody{Message: msg, Type: translate.OpenAIErrorType(status)}}
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeStreamError reports an error after a stream has started, when the
// HTTP status can no longer change.
func writeStreamError(w io.Writer, format, msg string) {
	if format == config.KindAnthropic {
		b, _ := json.Marshal(anthropic.ErrorResponse{
			Type: "error", Error: anthropic.ErrorBody{Type: "api_error", Message: msg},
		})
		_ = sse.Write(w, "error", b)
		return
	}
	b, _ := json.Marshal(openai.ErrorResponse{Error: openai.ErrorBody{Message: msg, Type: "server_error"}})
	_ = sse.Write(w, "", b)
}
