package metering

import (
	"sync"

	"github.com/pkoukk/tiktoken-go"
	tiktoken_loader "github.com/pkoukk/tiktoken-go-loader"
)

var (
	encOnce sync.Once
	enc     *tiktoken.Tiktoken
)

func encoding() *tiktoken.Tiktoken {
	encOnce.Do(func() {
		// The offline loader reads encodings embedded in the binary, so
		// counting never downloads anything at runtime.
		tiktoken.SetBpeLoader(tiktoken_loader.NewOfflineLoader())
		if e, err := tiktoken.GetEncoding(tiktoken.MODEL_O200K_BASE); err == nil {
			enc = e
		}
	})
	return enc
}

// WarmTokenizer loads the tokenizer's tables in the background, so the first
// call that needs a count does not wait for them.
func WarmTokenizer() { go encoding() }

// CountTokens counts tokens with the o200k_base encoding used by OpenAI's
// current models. Anthropic publishes no local tokenizer, so for Claude models
// this is an approximation; usage counted this way is always flagged
// Estimated. It falls back to EstimateTokens if the encoding cannot load.
func CountTokens(text string) int64 {
	if text == "" {
		return 0
	}
	if e := encoding(); e != nil {
		return int64(len(e.EncodeOrdinary(text)))
	}
	return EstimateTokens(len(text))
}
