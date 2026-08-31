// Package billing provides cost calculation from token counts and model pricing.
package billing

import (
	"sync"

	"github.com/aware/gateway/internal/config"
)

// PricingTable maps model names to per-token prices and provides
// cost calculation for audit records. Safe for concurrent use.
type PricingTable struct {
	mu      sync.RWMutex
	enabled bool
	models  map[string]config.ModelPrice
	def     *config.ModelPrice
}

func NewPricingTable(cfg config.PricingConfig) *PricingTable {
	return &PricingTable{
		enabled: cfg.Enabled,
		models:  cfg.Models,
		def:     cfg.Default,
	}
}

// Calculate returns the cost in USD for a request with the given model
// and token counts. Returns 0 when pricing is disabled, the model is
// unknown and no default is configured, or token counts are zero.
func (t *PricingTable) Calculate(model string, promptTokens, completionTokens int) float64 {
	if t == nil || !t.enabled {
		return 0
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	price, ok := t.lookup(model)
	if !ok {
		return 0
	}

	const perM = 1_000_000.0
	return float64(promptTokens)*price.Prompt/perM +
		float64(completionTokens)*price.Completion/perM
}

func (t *PricingTable) lookup(model string) (config.ModelPrice, bool) {
	if model == "" {
		if t.def != nil {
			return *t.def, true
		}
		return config.ModelPrice{}, false
	}

	// Exact match
	if p, ok := t.models[model]; ok {
		return p, true
	}

	// Prefix match: progressively shorter prefixes at /, -, . boundaries
	for i := len(model) - 1; i > 0; i-- {
		if model[i] == '/' || model[i] == '-' || model[i] == '.' {
			if p, ok := t.models[model[:i]]; ok {
				return p, true
			}
		}
	}

	// Default fallback
	if t.def != nil {
		return *t.def, true
	}

	return config.ModelPrice{}, false
}

func (t *PricingTable) Enabled() bool {
	return t != nil && t.enabled
}
