package main

import (
	"context"
	"math"
	"testing"

	"github.com/comnik/autoprobe/internal/provider"
)

// costProvider is a minimal Provider stub for exercising the per-model usage
// accumulators. Name()/DefaultModel() are configurable so the test can drive
// EstimatedCost against real pricingTable rows.
type costProvider struct {
	name, def string
}

func (p costProvider) Name() string         { return p.name }
func (p costProvider) DefaultModel() string { return p.def }
func (p costProvider) Generate(context.Context, string, provider.Context, provider.Options) (provider.AssistantMessage, error) {
	return provider.AssistantMessage{}, nil
}

func newCostAgent(name, def string) *Agent {
	return &Agent{provider: costProvider{name: name, def: def}, usageByModel: map[string]provider.Usage{}}
}

// TestAddUsagePerModel asserts usage folds into per-model buckets, TotalTokens
// splits out the cached portion, and EstimatedCost prices each model at its
// own rate.
func TestAddUsagePerModel(t *testing.T) {
	a := newCostAgent("anthropic", "claude-opus-4-7")

	// Two distinct models in one run — cost must price each at its own rate,
	// not a single global default.
	a.addUsage("claude-opus-4-7", provider.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10})
	a.addUsage("claude-opus-4-7", provider.Usage{InputTokens: 100, OutputTokens: 50, CacheWriteInputTokens: 20})
	a.addUsage("claude-haiku-4-5", provider.Usage{InputTokens: 1000, OutputTokens: 200})

	in, cached, out := a.TotalTokens()
	// in = full-price input + cache buckets across both models.
	if wantIn := (100 + 100 + 10 + 20) + 1000; in != wantIn {
		t.Errorf("in = %d, want %d", in, wantIn)
	}
	if wantCached := 10 + 20; cached != wantCached {
		t.Errorf("cached = %d, want %d", cached, wantCached)
	}
	if wantOut := 50 + 50 + 200; out != wantOut {
		t.Errorf("out = %d, want %d", out, wantOut)
	}

	usd, complete := a.EstimatedCost()
	if !complete {
		t.Fatal("complete = false, want true (all models priced)")
	}
	opus, _ := lookupPrice("anthropic", "claude-opus-4-7")
	haiku, _ := lookupPrice("anthropic", "claude-haiku-4-5")
	want := estimateCost(opus, provider.Usage{InputTokens: 200, OutputTokens: 100, CacheReadInputTokens: 10, CacheWriteInputTokens: 20}) +
		estimateCost(haiku, provider.Usage{InputTokens: 1000, OutputTokens: 200})
	if math.Abs(usd-want) > 1e-9 {
		t.Errorf("EstimatedCost = %v, want %v", usd, want)
	}
}

// TestEstimatedCostIncompleteOnUnknownModel asserts that a bucket whose model
// isn't in pricingTable flips complete=false, so the TUI prints "—" rather
// than a silently-undercounted figure.
func TestEstimatedCostIncompleteOnUnknownModel(t *testing.T) {
	a := newCostAgent("anthropic", "claude-opus-4-7")
	a.addUsage("claude-opus-4-7", provider.Usage{InputTokens: 100})
	a.addUsage("some-future-model", provider.Usage{InputTokens: 100})
	if _, complete := a.EstimatedCost(); complete {
		t.Error("complete = true, want false (one bucket is an unpriced model)")
	}
}

// TestAddUsageEmptyModelFallsBackToDefault asserts a provider that doesn't echo
// a model id still lands its tokens in a priceable bucket (the provider
// default) rather than an empty-string key.
func TestAddUsageEmptyModelFallsBackToDefault(t *testing.T) {
	a := newCostAgent("anthropic", "claude-opus-4-7")
	a.addUsage("", provider.Usage{InputTokens: 100, OutputTokens: 10})
	if _, ok := a.usageByModel[""]; ok {
		t.Error("usage landed under empty-string key; expected fallback to default model")
	}
	if _, complete := a.EstimatedCost(); !complete {
		t.Error("complete = false; fallback default should be priceable")
	}
}
