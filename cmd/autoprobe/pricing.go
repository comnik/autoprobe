package main

import (
	"strings"

	"github.com/comnik/autoprobe/internal/provider"
)

// modelPrice is per-million-token USD pricing for a single model id. All four
// rates are full per-MTok prices (not multipliers); estimateCost prices each
// disjoint Usage bucket against its rate and sums.
type modelPrice struct {
	inputPerMTok      float64
	outputPerMTok     float64
	cacheReadPerMTok  float64 // prompt-cache hits; typically 0.1x input
	cacheWritePerMTok float64 // prompt-cache writes; 0 where the provider doesn't bill writes
}

// pricingTable is hand-maintained — there is no official machine-readable
// Anthropic price feed, so rates are transcribed from each provider's
// canonical pricing page and verified by TestPricingCoverage. Unknown
// (provider, model) pairs fall back to displaying "—" instead of a misleading
// number — better to know we don't know than to print a wrong figure.
//
// Keys are matched on (provider name, exact-or-prefix model id). Provider
// names MUST match Provider.Name() ("anthropic", "openai", "google", "grok").
// Prefix matching is intentional: Anthropic models like "claude-opus-4-7" and
// "claude-opus-4-7[1m]" share a pricing schedule, and the full 1M context
// ships at standard per-token pricing for the models we run (no >200K tier).
//
// Rates are transcribed from each provider's canonical pricing page (verified
// 2026-05-31). Per-provider notes and documented gaps:
//   - Anthropic: rates + 5-minute-TTL cache schedule from the pricing docs.
//   - Google: Gemini 2.5 standard (≤200K) input/output/cache-read. Gemini Pro
//     has a >200K surcharge ($1.25→$2.50 in, $10→$15 out) that the flat
//     prefix rate ignores, so long-context Pro runs UNDERCOUNT; cache storage
//     (per-hour) is also out of scope. Both are accepted, documented gaps.
//   - OpenAI: gpt-5.x rates incl. published cached-input; no cache-write
//     surcharge (automatic caching), so cacheWrite is 0.
//   - xAI: docs.x.ai lists input/output but NOT a cache-read discount, so
//     cache reads are priced at the full input rate — a conservative upper
//     bound rather than an aggregator guess. Revise if xAI publishes one.
//
// Order matters where one id is a prefix of another: more specific rows must
// come first (e.g. gpt-5.4-mini before gpt-5.4) so the cheaper variant isn't
// shadowed by the broader prefix.
var pricingTable = []struct {
	provider string
	prefix   string
	price    modelPrice
}{
	// inputPerMTok, outputPerMTok, cacheReadPerMTok, cacheWritePerMTok
	{"anthropic", "claude-opus-4", modelPrice{5.0, 25.0, 0.50, 6.25}},
	{"anthropic", "claude-sonnet-4", modelPrice{3.0, 15.0, 0.30, 3.75}},
	{"anthropic", "claude-haiku-4", modelPrice{1.0, 5.0, 0.10, 1.25}},
	{"google", "gemini-2.5-pro", modelPrice{1.25, 10.0, 0.125, 0}},
	{"google", "gemini-2.5-flash", modelPrice{0.30, 2.50, 0.03, 0}},
	{"openai", "gpt-5.3-codex", modelPrice{1.75, 14.0, 0.175, 0}},
	{"openai", "gpt-5.5", modelPrice{5.0, 30.0, 0.50, 0}},
	{"openai", "gpt-5.4-mini", modelPrice{0.75, 4.50, 0.075, 0}}, // before gpt-5.4
	{"openai", "gpt-5.4", modelPrice{2.50, 15.0, 0.25, 0}},
	{"grok", "grok-4.3", modelPrice{1.25, 2.50, 1.25, 0}}, // cache read = input (xAI publishes no discount)
	{"grok", "grok-build-0.1", modelPrice{1.00, 2.00, 1.00, 0}},
}

// lookupPrice returns the per-million-token price entry for a given
// provider/model pair, and whether one was found.
func lookupPrice(providerName, model string) (modelPrice, bool) {
	for _, e := range pricingTable {
		if e.provider == providerName && strings.HasPrefix(model, e.prefix) {
			return e.price, true
		}
	}
	return modelPrice{}, false
}

// estimateCost computes the dollar cost for one model's usage, pricing each
// disjoint bucket (full-price input, output, cache read, cache write) against
// its rate and summing.
func estimateCost(p modelPrice, u provider.Usage) float64 {
	return (float64(u.InputTokens)*p.inputPerMTok +
		float64(u.OutputTokens)*p.outputPerMTok +
		float64(u.CacheReadInputTokens)*p.cacheReadPerMTok +
		float64(u.CacheWriteInputTokens)*p.cacheWritePerMTok) / 1_000_000
}
