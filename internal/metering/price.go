package metering

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Nanos is an amount of money in billionths of a US dollar. Integer
// nanodollars keep budget counters exact: one cached token on the cheapest
// models costs about 1e-8 USD, which a micro-dollar unit would round to zero.
type Nanos int64

// FromUSD converts dollars to nanodollars.
func FromUSD(usd float64) Nanos { return Nanos(math.Round(usd * 1e9)) }

// USD converts to dollars.
func (n Nanos) USD() float64 { return float64(n) / 1e9 }

// String formats for people, for example "$1.2346".
func (n Nanos) String() string { return fmt.Sprintf("$%.4f", n.USD()) }

// Rates are prices in USD per million tokens.
type Rates struct {
	Input        float64 `json:"input" yaml:"input"`
	CacheWrite   float64 `json:"cache_write" yaml:"cache_write"`       // 5-minute lifetime
	CacheWrite1h float64 `json:"cache_write_1h" yaml:"cache_write_1h"` // 1-hour lifetime
	CachedRead   float64 `json:"cached_read" yaml:"cached_read"`
	Output       float64 `json:"output" yaml:"output"`
}

func (r Rates) write1h() float64 {
	if r.CacheWrite1h > 0 {
		return r.CacheWrite1h
	}
	return r.CacheWrite
}

// Price is one row of the price table: what a model costs from a date on.
type Price struct {
	// Model is an exact model name as sent to the provider, or a prefix
	// ending in "*".
	Model string `json:"model"`
	// Speed is "" for standard calls or "fast" for a provider's premium
	// fast mode, which has its own rates.
	Speed string `json:"speed,omitempty"`
	Rates
	// LongContext replaces Rates for calls whose prompt (all input tokens,
	// cache reads and writes included) is over LongContextThreshold.
	LongContextThreshold int64  `json:"long_context_threshold,omitempty"`
	LongContext          *Rates `json:"long_context,omitempty"`
	// USMultiplier scales the whole cost when the provider reports US-only
	// inference (Anthropic's inference_geo "us"). Zero means it does not
	// apply.
	USMultiplier  float64   `json:"us_multiplier,omitempty"`
	EffectiveFrom time.Time `json:"effective_from"`
}

func (p Price) rates(promptTokens int64) Rates {
	if p.LongContext != nil && p.LongContextThreshold > 0 && promptTokens > p.LongContextThreshold {
		return *p.LongContext
	}
	return p.Rates
}

// Cost prices a call's usage:
//
//	cost = (T_in·P_in + T_cached·P_cached + T_write·P_write + T_write1h·P_write1h + T_out·P_out) / 10^6
//
// in USD, returned in nanodollars.
func (p Price) Cost(u Usage) Nanos {
	r := p.rates(u.PromptTokens())
	perMillion := float64(u.InputTokens)*r.Input +
		float64(u.CachedReadTokens)*r.CachedRead +
		float64(u.CacheWriteTokens)*r.CacheWrite +
		float64(u.CacheWrite1hTokens)*r.write1h() +
		float64(u.OutputTokens)*r.Output
	if u.InferenceGeo == "us" && p.USMultiplier > 0 {
		perMillion *= p.USMultiplier
	}
	// USD per million tokens to nanodollars per token is a factor of 1e3.
	return Nanos(math.Round(perMillion * 1e3))
}

// MaxCost is the most a call can cost before the provider answers: the
// whole prompt billed at the dearest input rate (a 1-hour cache write costs
// more than plain input), maxOutput tokens of output, and the US multiplier.
func (p Price) MaxCost(promptTokens, maxOutput int64) Nanos {
	r := p.rates(promptTokens)
	in := max(r.Input, r.CacheWrite, r.write1h(), r.CachedRead)
	perMillion := float64(promptTokens)*in + float64(maxOutput)*r.Output
	if p.USMultiplier > 1 {
		perMillion *= p.USMultiplier
	}
	return Nanos(math.Ceil(perMillion * 1e3))
}

// PriceBook answers "what did this model cost at this time". It is safe for
// concurrent use and can be replaced wholesale when the table changes.
type PriceBook struct {
	mu       sync.RWMutex
	exact    map[priceKey][]Price // sorted by EffectiveFrom, oldest first
	patterns []Price              // trailing-"*" rows, longest prefix first
}

type priceKey struct{ model, speed string }

// NewPriceBook builds a book from price rows.
func NewPriceBook(prices []Price) *PriceBook {
	b := &PriceBook{}
	b.Replace(prices)
	return b
}

// Replace swaps in a new set of price rows.
func (b *PriceBook) Replace(prices []Price) {
	exact := map[priceKey][]Price{}
	var patterns []Price
	for _, p := range prices {
		if strings.HasSuffix(p.Model, "*") {
			patterns = append(patterns, p)
			continue
		}
		k := priceKey{p.Model, p.Speed}
		exact[k] = append(exact[k], p)
	}
	for _, rows := range exact {
		sort.Slice(rows, func(i, j int) bool { return rows[i].EffectiveFrom.Before(rows[j].EffectiveFrom) })
	}
	sort.SliceStable(patterns, func(i, j int) bool {
		if len(patterns[i].Model) != len(patterns[j].Model) {
			return len(patterns[i].Model) > len(patterns[j].Model)
		}
		return patterns[i].EffectiveFrom.After(patterns[j].EffectiveFrom)
	})
	b.mu.Lock()
	b.exact, b.patterns = exact, patterns
	b.mu.Unlock()
}

// dateSuffix matches the snapshot dates providers append to model names,
// as in claude-sonnet-4-5-20250929 or gpt-4o-2024-08-06.
var dateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)

// Lookup finds the price in effect for a model at a time: an exact row, then
// the same name without a snapshot date, then the longest matching prefix.
func (b *PriceBook) Lookup(model, speed string, at time.Time) (Price, bool) {
	if b == nil {
		return Price{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, name := range []string{model, dateSuffix.ReplaceAllString(model, "")} {
		if p, ok := latest(b.exact[priceKey{name, speed}], at); ok {
			return p, true
		}
	}
	for _, p := range b.patterns {
		if p.Speed == speed && strings.HasPrefix(model, strings.TrimSuffix(p.Model, "*")) && !p.EffectiveFrom.After(at) {
			return p, true
		}
	}
	return Price{}, false
}

func latest(rows []Price, at time.Time) (Price, bool) {
	for i := len(rows) - 1; i >= 0; i-- {
		if !rows[i].EffectiveFrom.After(at) {
			return rows[i], true
		}
	}
	return Price{}, false
}

// All returns every row, for listing.
func (b *PriceBook) All() []Price {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []Price
	for _, rows := range b.exact {
		out = append(out, rows...)
	}
	out = append(out, b.patterns...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		if out[i].Speed != out[j].Speed {
			return out[i].Speed < out[j].Speed
		}
		return out[i].EffectiveFrom.Before(out[j].EffectiveFrom)
	})
	return out
}
