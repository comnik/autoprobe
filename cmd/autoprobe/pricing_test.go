package main

import (
	"math"
	"testing"

	"github.com/comnik/autoprobe/internal/provider"
)

func TestEstimateCost(t *testing.T) {
	// Opus 4.7 schedule: $5 in / $25 out / $0.50 cache-read / $6.25 cache-write.
	p := modelPrice{inputPerMTok: 5.0, outputPerMTok: 25.0, cacheReadPerMTok: 0.50, cacheWritePerMTok: 6.25}
	const eps = 1e-9
	cases := []struct {
		name string
		u    provider.Usage
		want float64
	}{
		{"input only", provider.Usage{InputTokens: 1_000_000}, 5.0},
		{"output only", provider.Usage{OutputTokens: 1_000_000}, 25.0},
		{"cache read discount", provider.Usage{CacheReadInputTokens: 1_000_000}, 0.50},
		{"cache write premium", provider.Usage{CacheWriteInputTokens: 1_000_000}, 6.25},
		{
			// All four buckets priced independently and summed.
			"mixed",
			provider.Usage{InputTokens: 200_000, OutputTokens: 100_000, CacheReadInputTokens: 800_000, CacheWriteInputTokens: 50_000},
			200_000*5.0/1e6 + 100_000*25.0/1e6 + 800_000*0.50/1e6 + 50_000*6.25/1e6,
		},
		{"zero", provider.Usage{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := estimateCost(p, tc.u); math.Abs(got-tc.want) > eps {
				t.Errorf("estimateCost = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEstimateCostCacheReadIsDiscounted is a guard against a future edit that
// accidentally prices cache reads at the full input rate — the whole reason
// caching saves money.
func TestEstimateCostCacheReadIsDiscounted(t *testing.T) {
	p, ok := lookupPrice("anthropic", "claude-opus-4-7")
	if !ok {
		t.Fatal("expected a price for claude-opus-4-7")
	}
	full := estimateCost(p, provider.Usage{InputTokens: 1_000_000})
	cached := estimateCost(p, provider.Usage{CacheReadInputTokens: 1_000_000})
	if cached >= full {
		t.Errorf("cache read cost %v should be cheaper than full input %v", cached, full)
	}
	write := estimateCost(p, provider.Usage{CacheWriteInputTokens: 1_000_000})
	if write <= full {
		t.Errorf("cache write cost %v should be pricier than full input %v", write, full)
	}
}

// TestPricingCoverage asserts every model the user can pick — each provider's
// selectable list (init_tui.go) plus its default — resolves to a price. This
// fails loudly when a model id is bumped past its table entry (the stale-Opus
// class of bug) or when a new table row's provider-name key is mistyped
// (silent fall-through to "—").
func TestPricingCoverage(t *testing.T) {
	// Provider names here MUST match Provider.Name(); note xAI's name is
	// "grok", not "xai". Defaults mirror each provider's NewX("") fallback.
	defaults := map[string]string{
		"anthropic": "claude-opus-4-7",
		"openai":    "gpt-5.3-codex",
		"google":    "gemini-2.5-pro",
		"grok":      "grok-4.3",
	}
	for prov, def := range defaults {
		models := []string{def}
		for _, mc := range suggestedModels(prov) {
			if mc.id == "" || mc.id == "__custom__" {
				continue // "(provider default)" / "Custom…" carry no concrete id
			}
			models = append(models, mc.id)
		}
		for _, model := range models {
			if _, ok := lookupPrice(prov, model); !ok {
				t.Errorf("%s/%s: no price found — every selectable model and provider default must be priced", prov, model)
			}
		}
	}
}

// TestPricingPrefixOrdering guards the prefix-collision ordering: a cheaper
// variant whose id is a prefix-suffix of a broader entry (gpt-5.4-mini vs
// gpt-5.4) must resolve to its own rate, not the broader one's.
func TestPricingPrefixOrdering(t *testing.T) {
	mini, _ := lookupPrice("openai", "gpt-5.4-mini")
	full, _ := lookupPrice("openai", "gpt-5.4")
	if mini.inputPerMTok >= full.inputPerMTok {
		t.Errorf("gpt-5.4-mini input %v should be cheaper than gpt-5.4 %v — prefix ordering shadowed the mini row",
			mini.inputPerMTok, full.inputPerMTok)
	}
}
