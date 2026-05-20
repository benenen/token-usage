package server

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
)

// Rate is per-1M-tokens USD.
type Rate struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	CacheCreation float64 `json:"cache_creation"`
	CacheRead     float64 `json:"cache_read"`
}

// DefaultRates returns a copy of the in-memory price table used as the
// initial seed for model_prices and as a fallback for models not yet
// known to the DB (e.g. brand-new SKU between LiteLLM syncs).
func DefaultRates() map[string]Rate {
	out := make(map[string]Rate, len(defaultRates))
	for k, v := range defaultRates {
		out[k] = v
	}
	return out
}

// defaultRates: keys are model-name prefixes. Longest prefix wins.
// Anthropic numbers are public list; OpenAI numbers are public list too
// except GPT-5.x which is approximate (override with --pricing JSON).
// CacheCreation=0 for OpenAI: their prompt cache has no write-time charge.
var defaultRates = map[string]Rate{
	// Anthropic
	"claude-opus-4":     {Input: 15, Output: 75, CacheCreation: 18.75, CacheRead: 1.50},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheCreation: 3.75, CacheRead: 0.30},
	"claude-haiku-4":    {Input: 1, Output: 5, CacheCreation: 1.25, CacheRead: 0.10},
	"claude-3-5-sonnet": {Input: 3, Output: 15, CacheCreation: 3.75, CacheRead: 0.30},
	"claude-3-5-haiku":  {Input: 0.80, Output: 4, CacheCreation: 1.00, CacheRead: 0.08},
	"claude-3-opus":     {Input: 15, Output: 75, CacheCreation: 18.75, CacheRead: 1.50},
	"claude-3-haiku":    {Input: 0.25, Output: 1.25, CacheCreation: 0.30, CacheRead: 0.03},

	// OpenAI
	"gpt-5":         {Input: 3, Output: 15, CacheCreation: 0, CacheRead: 0.30}, // approximate — override per your contract
	"gpt-5-mini":    {Input: 0.30, Output: 1.20, CacheCreation: 0, CacheRead: 0.03},
	"gpt-4o":        {Input: 2.50, Output: 10, CacheCreation: 0, CacheRead: 1.25},
	"gpt-4o-mini":   {Input: 0.15, Output: 0.60, CacheCreation: 0, CacheRead: 0.075},
	"gpt-4-turbo":   {Input: 10, Output: 30, CacheCreation: 0, CacheRead: 0},
	"gpt-4":         {Input: 30, Output: 60, CacheCreation: 0, CacheRead: 0},
	"gpt-3.5":       {Input: 0.50, Output: 1.50, CacheCreation: 0, CacheRead: 0},
}

type Pricer struct {
	mu       sync.RWMutex
	rates    map[string]Rate
	prefixes []string // sorted by length desc for longest-prefix match
}

func NewPricer(path string) (*Pricer, error) {
	p := &Pricer{rates: make(map[string]Rate)}
	for k, v := range defaultRates {
		p.rates[k] = v
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var custom map[string]Rate
		if err := json.Unmarshal(data, &custom); err != nil {
			return nil, err
		}
		for k, v := range custom {
			p.rates[k] = v
		}
	}
	p.reindex()
	return p, nil
}

func (p *Pricer) reindex() {
	p.prefixes = p.prefixes[:0]
	for k := range p.rates {
		p.prefixes = append(p.prefixes, k)
	}
	sort.Slice(p.prefixes, func(i, j int) bool {
		return len(p.prefixes[i]) > len(p.prefixes[j])
	})
}

// Cost returns USD for the given token counts under the given model.
// Returns 0 if no matching rate exists (caller can flag as unknown model).
func (p *Pricer) Cost(model string, in, out, cc, cr int64) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r, ok := p.rates[model]
	if !ok {
		for _, prefix := range p.prefixes {
			if strings.HasPrefix(model, prefix) {
				r = p.rates[prefix]
				ok = true
				break
			}
		}
	}
	if !ok {
		return 0
	}
	const per = 1_000_000.0
	return float64(in)*r.Input/per +
		float64(out)*r.Output/per +
		float64(cc)*r.CacheCreation/per +
		float64(cr)*r.CacheRead/per
}
