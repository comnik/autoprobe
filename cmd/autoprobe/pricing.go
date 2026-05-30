package main

import "strings"

// modelPrice is per-million-token USD pricing for a single model id. Cache
// hits and prompt-cache writes are deferred — the Usage struct doesn't carry
// that breakdown yet, so the cost line is labelled "est. cost" and is an
// upper bound for providers that bill cache hits at a discount.
type modelPrice struct {
	inputPerMTok  float64
	outputPerMTok float64
}

// pricingTable is hand-maintained. Unknown (provider, model) pairs fall back
// to displaying "—" instead of a misleading number — better to know we
// don't know than to print a wrong figure.
//
// Keys are matched on (provider name, exact-or-prefix model id). Prefix
// matching is intentional: Anthropic models like "claude-opus-4-7" and
// "claude-opus-4-7[1m]" share a pricing schedule for the input/output
// figures we track here.
var pricingTable = []struct {
	provider string
	prefix   string
	price    modelPrice
}{
	{"anthropic", "claude-opus-4", modelPrice{15.0, 75.0}},
	{"anthropic", "claude-sonnet-4", modelPrice{3.0, 15.0}},
	{"anthropic", "claude-haiku-4", modelPrice{1.0, 5.0}},
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

// estimateCost computes the dollar cost for the given token totals.
func estimateCost(p modelPrice, inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.inputPerMTok/1_000_000 + float64(outputTokens)*p.outputPerMTok/1_000_000
}
