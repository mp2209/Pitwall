package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mp2209/pitwall/internal/metering"
)

type priceFile struct {
	Prices []priceEntry `yaml:"prices"`
}

type priceEntry struct {
	Model          string `yaml:"model"`
	Speed          string `yaml:"speed"`
	metering.Rates `yaml:",inline"`
	USMultiplier   float64 `yaml:"us_multiplier"`
	EffectiveFrom  string  `yaml:"effective_from"`
	LongContext    *struct {
		Threshold      int64 `yaml:"threshold"`
		metering.Rates `yaml:",inline"`
	} `yaml:"long_context"`
}

// LoadPrices reads a price table file.
func LoadPrices(file string) ([]metering.Price, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return ParsePrices(b)
}

// ParsePrices decodes and checks a price table.
func ParsePrices(b []byte) ([]metering.Price, error) {
	var f priceFile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse prices: %w", err)
	}
	var (
		out  []metering.Price
		errs []error
		seen = map[string]bool{}
	)
	for i, e := range f.Prices {
		p := metering.Price{Model: e.Model, Speed: e.Speed, Rates: e.Rates, USMultiplier: e.USMultiplier}
		if e.EffectiveFrom != "" {
			t, err := time.Parse("2006-01-02", e.EffectiveFrom)
			if err != nil {
				errs = append(errs, fmt.Errorf("prices[%d] (%s): effective_from must be YYYY-MM-DD", i, e.Model))
			}
			p.EffectiveFrom = t
		}
		if e.LongContext != nil {
			r := e.LongContext.Rates
			p.LongContextThreshold, p.LongContext = e.LongContext.Threshold, &r
			if p.LongContextThreshold <= 0 {
				errs = append(errs, fmt.Errorf("prices[%d] (%s): long_context needs a threshold", i, e.Model))
			}
		}
		switch {
		case e.Model == "":
			errs = append(errs, fmt.Errorf("prices[%d]: model is empty", i))
		case e.Speed != "" && e.Speed != "fast":
			errs = append(errs, fmt.Errorf("prices[%d] (%s): speed must be empty or fast", i, e.Model))
		case e.Input <= 0 && e.Output <= 0:
			errs = append(errs, fmt.Errorf("prices[%d] (%s): input and output rates are missing", i, e.Model))
		}
		id := e.Model + "|" + e.Speed + "|" + e.EffectiveFrom
		if seen[id] {
			errs = append(errs, fmt.Errorf("prices[%d] (%s): duplicate row", i, e.Model))
		}
		seen[id] = true
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}
