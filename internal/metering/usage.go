// Package metering holds token accounting. Pricing and budgets land here in
// Phase 2.
package metering

// Usage is the token count for one model call, normalized across providers.
//
// InputTokens is uncached input only. OpenAI reports prompt_tokens including
// cached tokens, so its adapter subtracts cached_tokens; Anthropic already
// reports input_tokens without cache reads and writes.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	CachedReadTokens int64 `json:"cached_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	// Estimated is true when the provider sent no usage and the numbers
	// were counted by the gateway.
	Estimated bool `json:"estimated"`
}

// PromptTokens is all input, cached or not.
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CachedReadTokens + u.CacheWriteTokens
}

// IsZero reports whether no tokens were recorded.
func (u Usage) IsZero() bool {
	return u.PromptTokens() == 0 && u.OutputTokens == 0
}

// EstimateTokens approximates a token count from a byte length, at roughly
// four bytes per token for English text and code.
//
// TODO(phase 2): replace with a real tokenizer for the model family.
func EstimateTokens(bytes int) int64 {
	if bytes <= 0 {
		return 0
	}
	return int64((bytes + 3) / 4)
}
