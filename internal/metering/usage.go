// Package metering holds token accounting, pricing and budgets.
package metering

// Usage is the token count for one model call, normalized across providers.
//
// InputTokens is uncached input only. OpenAI reports prompt_tokens including
// cached tokens, so its adapter subtracts cached_tokens; Anthropic already
// reports input_tokens without cache reads and writes.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	CachedReadTokens int64 `json:"cached_read_tokens"`
	// CacheWriteTokens are written with the default 5-minute lifetime;
	// CacheWrite1hTokens with the 1-hour lifetime, which costs more.
	CacheWriteTokens   int64 `json:"cache_write_tokens"`
	CacheWrite1hTokens int64 `json:"cache_write_1h_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	// Speed is "fast" when the provider served the call in its premium
	// fast mode (priced separately); empty otherwise.
	Speed string `json:"speed,omitempty"`
	// InferenceGeo is where the provider says inference ran, when it says
	// ("us" carries a price multiplier on some models).
	InferenceGeo string `json:"inference_geo,omitempty"`
	// Estimated is true when the provider sent no usage and the numbers
	// were counted by the gateway.
	Estimated bool `json:"estimated"`
}

// CacheWrites is all tokens written to the cache, either lifetime.
func (u Usage) CacheWrites() int64 { return u.CacheWriteTokens + u.CacheWrite1hTokens }

// PromptTokens is all input, cached or not.
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CachedReadTokens + u.CacheWrites()
}

// IsZero reports whether no tokens were recorded.
func (u Usage) IsZero() bool {
	return u.PromptTokens() == 0 && u.OutputTokens == 0
}

// EstimateTokens approximates a token count from a byte length, at roughly
// four bytes per token for English text and code. It is cheap enough for
// every request, so budget reservations use it; CountTokens is the more
// accurate count for recorded usage.
func EstimateTokens(bytes int) int64 {
	if bytes <= 0 {
		return 0
	}
	return int64((bytes + 3) / 4)
}
