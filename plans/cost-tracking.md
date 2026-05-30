# Cost tracking: cache-aware, per-model accuracy

## Status

**Implemented** (2026-05-31). All three sections landed. Notes on where
reality diverged from the planning assumptions:

- **xAI's `Provider.Name()` is `"grok"`, not `"xai"`** — the exact trap §2
  warned about. Table keys use `"grok"`.
- **The exposed OpenAI/xAI model ids were themselves stale**, which is why
  pricing was initially missing. `init_tui.go` offered `gpt-5-codex`/`gpt-5`/
  `o3` and `grok-4`/`grok-4-fast-reasoning`/`grok-code-fast-1` — none on the
  canonical pricing pages anymore (xAI deprecated those slugs 2026-05-15, now
  redirecting to `grok-4.3`). Rather than guess prices for dead ids, the menus
  *and* provider defaults were refreshed to current models and those priced:
  - OpenAI: `gpt-5.3-codex` (default), `gpt-5.5`, `gpt-5.4`, `gpt-5.4-mini` —
    rates incl. published cached-input; no cache-write surcharge.
  - xAI: `grok-4.3` (default), `grok-build-0.1`. docs.x.ai publishes no
    cache-read discount, so cache reads are priced at the full input rate (a
    documented upper bound, not an aggregator guess).
  The `unpriced` allowlist is therefore gone — every selectable model is now
  priced, and `TestPricingCoverage` requires it (fails loudly on the next
  stale-id drift). A `TestPricingPrefixOrdering` guard covers the
  `gpt-5.4-mini` vs `gpt-5.4` prefix-collision ordering.
- **Anthropic rates confirmed** against the docs ($5/$25/$0.50/$6.25 Opus,
  $3/$15/$0.30/$3.75 Sonnet, $1/$5/$0.10/$1.25 Haiku — exactly the planned
  figures). **Google priced** from the canonical page: Gemini 2.5 Pro
  $1.25/$10/$0.125 (≤200K), Flash $0.30/$2.50/$0.03; Pro's >200K surcharge and
  Google cache storage remain documented undercounts.
- **Subset normalization extracted** to `provider.usageFromSubset` (shared by
  OpenAI/xAI/Google) and unit-tested for the disjoint invariant; Anthropic
  copies fields directly.

Original plan (two accuracy fixes to the TUI "est. cost" line ahead of
enabling prompt caching) follows below.

## Problem

The TUI shows an estimated dollar cost (`tui.go:renderTokens`,
`pricing.go`). The token counts feeding it are exact — they come straight
from provider-reported `Usage` (`msg.Usage.InputTokens` /
`OutputTokens`, accumulated into two atomics in `agent.go:707` and
`agent.go:886`). The dollar figure, however, has three gaps:

1. **No cache accounting.** `provider.Usage` only carries plain
   `InputTokens` / `OutputTokens`. Anthropic returns cache reads and cache
   writes in *separate* fields that we never read. The code comment in
   `pricing.go` calls the estimate an "upper bound for providers that bill
   cache hits at a discount" — but that framing is wrong: in Anthropic's
   API, cache-read and cache-write tokens are *excluded* from
   `input_tokens`, so once caching is enabled those tokens won't be counted
   at all and the estimate will *under*count, not overcount. We're about to
   enable caching, so this stops being hypothetical.

2. **Cost uses `DefaultModel()`, not the model actually used.** The TUI
   prices the run's full token total against
   `m.agent.Provider().DefaultModel()` (`tui.go:349`). Tokens are summed
   across every inference pass regardless of which model produced them. If
   a run ever mixes models, the rate applied is wrong. The actual model id
   is already available — every provider populates
   `AssistantMessage.Model` from its response (`anthropic.go:81`) — we just
   discard it for pricing.

3. **The hardcoded table is already stale.** `pricingTable`
   (`pricing.go:27`) prices `claude-opus-4` at $15/$75. That was Opus
   4/4.1 pricing; Opus 4.7 is **$5/$25** per MTok as of May 2026. The
   prefix `claude-opus-4` matches `claude-opus-4-7`, so we're currently
   overcharging Opus runs by 3x. This is the cost of hand-maintenance and
   motivates the "rates source" question below.

## Is there a pricing API so we don't hardcode rates?

Short answer: **no official one.** Anthropic does not publish a
machine-readable price feed. The `/v1/models` endpoint lists model ids and
display names but carries **no pricing**. The public price list lives only
on the docs/pricing web page, which is not a stable API.

The realistic options:

| Option | What it is | Trade-off |
| --- | --- | --- |
| **Keep hardcoded** (status quo) | Hand-maintained `pricingTable` | Zero deps, zero network. Goes stale silently (see gap #3). |
| **Vendor a community feed** | LiteLLM's `model_prices_and_context_window.json` or models.dev — both are JSON price tables covering all providers, including cache read/write rates | Broad coverage, machine-readable, but a third party we'd trust for dollar figures; needs a vendoring/refresh step. Not authoritative — community feeds also lag and occasionally carry errors. |
| **Scrape the pricing page** | Fetch + parse the docs pricing page | Brittle (HTML layout), and still not contractual. Not recommended. |

Recommendation: **stay hardcoded for now, but treat it as a known
liability.** A hand-maintained table is fine for the three Anthropic models
we run if we (a) fix the stale Opus row, (b) add cache read/write rates to
the struct so the table is complete once caching lands, and (c) add a test
that the table covers each provider's `DefaultModel()` so a model bump
fails CI loudly instead of silently mispricing. If coverage later needs to
span many providers/models, revisit vendoring a community feed behind a
small adapter — but that's a follow-up, not a blocker.

## Design

### 1. Plumb cache tokens through `Usage` — disjoint-bucket invariant

Extend `provider.Usage` (`internal/provider/types.go:120`):

```go
type Usage struct {
	InputTokens           int // full-price input only — EXCLUDES the cache buckets below
	OutputTokens          int
	CacheReadInputTokens  int // tokens served from prompt cache (billed at a discount)
	CacheWriteInputTokens int // tokens written to prompt cache (billed at a premium; 0 where the provider doesn't bill writes)
}
```

**The contract is that the three input buckets are *disjoint*** —
`InputTokens` is full-price input *only*, and the two cache fields are
separate. `estimateCost` can then price each bucket independently and sum,
with no double-counting. This invariant is the load-bearing decision here,
because providers report cache tokens in two different shapes:

| Provider | What the SDK reports | Adapter must do |
| --- | --- | --- |
| **Anthropic** | `input_tokens` already *excludes* cache read/write (disjoint) | Copy fields straight across — already matches the invariant. |
| **OpenAI / xAI** | `input_tokens` *includes* `…_details.cached_tokens` (subset) | Subtract: `InputTokens = total − cached`, `CacheReadInputTokens = cached`. |
| **Google** | `prompt_token_count` *includes* `cached_content_token_count` (subset) | Same subtraction. |

So every provider normalizes to the disjoint invariant at its own boundary;
downstream code (accumulation, pricing, TUI) never has to know which shape
the provider used.

Anthropic (`anthropic.go:82`) — already disjoint, just copy:

```go
out.Usage.InputTokens = int(resp.Usage.InputTokens)
out.Usage.OutputTokens = int(resp.Usage.OutputTokens)
out.Usage.CacheReadInputTokens = int(resp.Usage.CacheReadInputTokens)
out.Usage.CacheWriteInputTokens = int(resp.Usage.CacheCreationInputTokens)
```

OpenAI (`openai.go:93`) — subtract the cached subset (Responses API exposes
`Usage.InputTokensDetails.CachedTokens`; OpenAI's automatic caching has no
write surcharge, so `CacheWriteInputTokens` stays 0):

```go
cached := int(resp.Usage.InputTokensDetails.CachedTokens)
out.Usage.InputTokens = int(resp.Usage.InputTokens) - cached
out.Usage.CacheReadInputTokens = cached
```

xAI (`grok.go:86`) — OpenAI-compatible, same subtraction via
`resp.Usage.PromptTokensDetails.CachedTokens`. Google (`google.go:83`) —
subtract `resp.UsageMetadata.CachedContentTokenCount` from
`PromptTokenCount`. (Confirm each field path against the pinned SDK version
at implementation time.)

Two billing wrinkles to record, not solve, in this pass:
- **OpenAI/xAI** don't charge a separate cache *write*; caching is automatic
  and writes are free, so `CacheWriteInputTokens = 0` is correct.
- **Google** bills cache *storage* per-token-per-hour, which can't be
  derived from a single response. We track read discount only; storage is
  out of scope and noted as a known undercount.

### 2. Cache-aware pricing

Extend `modelPrice` (`pricing.go:9`) with the two cache rates:

```go
type modelPrice struct {
	inputPerMTok      float64
	outputPerMTok     float64
	cacheReadPerMTok  float64 // typically 0.1x input
	cacheWritePerMTok float64 // typically 1.25x input for 5-min TTL
}
```

Fix the stale Opus row, fill in cache rates, and (per the decision below)
add rows for the other providers' default-and-selectable models. Models in
play, from `init_tui.go` and each provider's `DefaultModel`:

- Anthropic: `claude-opus-4-7`, `claude-sonnet-4-6`, `claude-haiku-4-5`
- OpenAI: `gpt-5-codex` (default), `gpt-5`, `o3`
- Google: `gemini-2.5-pro` (default), `gemini-2.5-flash`
- xAI: `grok-4` (default), `grok-4-fast-reasoning`, `grok-code-fast-1`

```go
// inputPerMTok, outputPerMTok, cacheReadPerMTok, cacheWritePerMTok
{"anthropic", "claude-opus-4",   modelPrice{5.0, 25.0, 0.50, 6.25}},
{"anthropic", "claude-sonnet-4", modelPrice{3.0, 15.0, 0.30, 3.75}},
{"anthropic", "claude-haiku-4",  modelPrice{1.0,  5.0, 0.10, 1.25}},
{"openai",    "gpt-5-codex",     modelPrice{ /* verify */ }},
{"openai",    "gpt-5",           modelPrice{ /* verify */ }},
{"openai",    "o3",              modelPrice{ /* verify */ }},
{"google",    "gemini-2.5-pro",  modelPrice{ /* verify */ }},
{"google",    "gemini-2.5-flash",modelPrice{ /* verify */ }},
{"xai",       "grok-4",          modelPrice{ /* verify */ }},
```

**Rates must be verified against each provider's official pricing page at
implementation time — do not trust the figures from this planning pass.**
The non-Anthropic price lists are volatile and the lineups churn (gpt-5.5,
grok-4.1, etc. already exist); third-party aggregators disagree. Pull each
number from the canonical source:
[Anthropic](https://platform.claude.com/docs/en/about-claude/pricing) ·
[OpenAI](https://openai.com/api/pricing/) ·
[Google](https://ai.google.dev/gemini-api/docs/pricing) ·
[xAI](https://docs.x.ai/developers/models). For Anthropic the 0.1x read /
1.25x write convention is the standard 5-minute-TTL schedule but should
still be confirmed per model. For providers whose `modelPrice` we can't pin
down confidently, leave the row out — an absent row shows `—` (honest "we
don't know"), which is the whole point of the lookup's miss behavior.

**Provider name keys must match `Provider.Name()`** (`"openai"`, `"google"`,
`"xai"`) — `lookupPrice` matches on provider name + model prefix, so a key
typo silently falls through to `—`. The coverage test below guards this.

**Gemini 2.5 Pro has a >200K-token surcharge** ($1.25→$2.50 input,
$10→$15 output above 200K) — unlike the current Anthropic models, where the
surcharge is gone (see "1M context" below). The flat prefix-matched rate
will *undercount* Gemini Pro runs that exceed 200K input. We accept that
undercount for now and document it; adding a per-entry threshold is a
follow-up if Gemini Pro long-context runs become common.

`estimateCost` (`pricing.go:44`) sums all four buckets:

```go
func estimateCost(p modelPrice, u provider.Usage) float64 {
	return (float64(u.InputTokens)*p.inputPerMTok +
		float64(u.OutputTokens)*p.outputPerMTok +
		float64(u.CacheReadInputTokens)*p.cacheReadPerMTok +
		float64(u.CacheWriteInputTokens)*p.cacheWritePerMTok) / 1_000_000
}
```

### 3. Price per actual model, not the default

The core change: stop summing tokens into two global atomics priced once at
render time. Instead accumulate **per-model usage buckets** and let the TUI
sum cost across them.

Replace the four global token atomics with a small mutex-guarded map on the
`Agent` keyed by model id:

```go
usageMu    sync.Mutex
usageByModel map[string]provider.Usage // keyed by AssistantMessage.Model
```

At each accumulation site (`agent.go:707`, `agent.go:886`), fold the message
usage into the bucket for `msg.Model`, falling back to
`a.provider.DefaultModel()` when `msg.Model` is empty (some providers may
not echo it):

```go
a.addUsage(msg.Model, msg.Usage)
```

`addUsage` locks, picks the key (`model`, or default if empty), and adds
each field. Contention is negligible — the TUI reads once per ~1s tick, the
loop writes once per inference pass.

Expose accumulators for the TUI:

- `TotalTokens() (in, cached, out int)` — sum across buckets. Per the TUI
  decision below, the cell breaks out the cached portion, so the renderer
  needs the cached count alongside the totals. `in` is the *total* input the
  model processed (full-price `InputTokens` + the cache buckets); `cached` is
  `CacheReadInputTokens + CacheWriteInputTokens`. The cell renders
  `tokens N in (M cached) / N out`.
- `EstimatedCost() (usd float64, complete bool)` — iterate buckets, look up
  each model's price, sum `estimateCost` per bucket. `complete` is false if
  any bucket's model is missing from `pricingTable`.

The TUI (`tui.go:renderTokens`) then calls `EstimatedCost()` directly and
renders `$%.4f` when `complete`, or `—` (optionally `~$X (partial)`) when a
model was unpriced — preserving the existing "refuse to print a wrong
number" behavior, now at per-model granularity.

This removes the `lookupPrice(provider.Name(), provider.DefaultModel())`
call from the TUI entirely; the provider's default model no longer factors
into cost.

## Testing

- `pricing_test.go`: table-driven `estimateCost` cases covering
  input-only, output, cache-read, cache-write, and mixed; assert the
  cache-read discount and cache-write premium land correctly.
- **Coverage guard:** a test asserting `lookupPrice` resolves *every* model
  id in `init_tui.go`'s selectable list (and each provider's
  `DefaultModel()`) — fails loudly when a model id is bumped past the table
  (directly addresses the stale-Opus class of bug) and when a new provider
  row's name key is mistyped.
- **Per-provider normalization:** for each provider adapter, feed a
  synthetic SDK response where cached tokens are reported in that provider's
  native shape (subset for OpenAI/Google/xAI, disjoint for Anthropic) and
  assert the resulting `Usage` satisfies the disjoint invariant — in
  particular that `InputTokens` is the *non-cached* remainder, so cost isn't
  double-counted.
- Agent-level: feed synthetic `AssistantMessage`s with distinct `Model`
  values and cache usage through the accumulation path; assert
  `EstimatedCost()` sums per-model and reports `complete=false` for an
  unknown model, and that `TotalTokens()` returns the expected
  `(in, cached, out)` split.

## Resolved decisions

These were the open questions; resolved with the facts gathered during
planning:

- **1M-context surcharge — non-issue for our Anthropic models.** The >200K
  premium tier only ever applied to Sonnet 4 / 4.5. The models we run
  (Opus 4.7, Sonnet 4.6) ship the full 1M context at standard per-token
  pricing, so the prefix-match flattening of `claude-…-4-7[1m]` is correct,
  not a bug. No threshold logic for Anthropic. (The one place a >200K
  surcharge still bites is **Gemini 2.5 Pro** — captured as a documented
  undercount in §2.)
- **Token cell breaks out cached tokens** → `tokens N in (M cached) / N out`
  (drives the `TotalTokens()` signature change in §3). Chosen so the
  cache-hit rate is visible while we validate that caching actually works.
- **Other providers priced now.** Add OpenAI/Google/xAI rows to the table
  and read their cached-token fields this pass (§1, §2). Per-model pricing
  already works for them since every adapter populates
  `AssistantMessage.Model`. Remaining provider-specific gaps — Google cache
  *storage* (per-hour, not in the response) and any provider whose rates we
  can't confidently pin — are left as documented undercounts / absent rows
  rather than guesses.
