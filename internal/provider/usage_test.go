package provider

import "testing"

// TestUsageFromSubset asserts the disjoint-bucket normalization for the
// providers (OpenAI, xAI, Google) that report cached tokens as a subset of
// their input total. The load-bearing property is that InputTokens ends up as
// the NON-cached remainder, so pricing each bucket independently can't
// double-count the cached tokens.
func TestUsageFromSubset(t *testing.T) {
	cases := []struct {
		name                  string
		totalInput, output    int
		cachedRead            int
		wantInput, wantCached int
	}{
		// totalInput is what the SDK reports as the input field; cachedRead is
		// the cached subset of it. InputTokens must come out as the remainder.
		{"openai responses", 1000, 200, 600, 400, 600},
		{"xai chat completions", 500, 50, 0, 500, 0}, // no cache hit: all full-price
		{"google gemini", 2048, 256, 2048, 0, 2048},  // fully cached: no full-price input
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := usageFromSubset(tc.totalInput, tc.output, tc.cachedRead)
			if u.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d (non-cached remainder)", u.InputTokens, tc.wantInput)
			}
			if u.CacheReadInputTokens != tc.wantCached {
				t.Errorf("CacheReadInputTokens = %d, want %d", u.CacheReadInputTokens, tc.wantCached)
			}
			if u.OutputTokens != tc.output {
				t.Errorf("OutputTokens = %d, want %d", u.OutputTokens, tc.output)
			}
			// Subset providers never carry a cache-write charge.
			if u.CacheWrite5mInputTokens != 0 || u.CacheWrite1hInputTokens != 0 {
				t.Errorf("cache writes = %d/%d, want 0/0", u.CacheWrite5mInputTokens, u.CacheWrite1hInputTokens)
			}
			// Disjoint invariant: full-price input + cache buckets == the
			// provider's reported input total.
			if got := u.InputTokens + u.CacheReadInputTokens + u.CacheWrite5mInputTokens + u.CacheWrite1hInputTokens; got != tc.totalInput {
				t.Errorf("buckets sum to %d, want %d (provider's input total)", got, tc.totalInput)
			}
		})
	}
}
