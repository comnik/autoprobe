// Package provider abstracts the model APIs autoprobe talks to (Anthropic,
// OpenAI, Google, xAI). Each provider translates the neutral Context shape
// to/from its native SDK, preserving signature fields so multi-turn
// reasoning round-trips.
package provider

import (
	"context"
	"encoding/json"
)

// Role identifies which side of the conversation a message belongs to.
type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolResult Role = "tool_result"
)

// Message is one entry in the conversation. Implementors are UserMessage,
// AssistantMessage, and ToolResultMessage.
type Message interface {
	Role() Role
	isMessage()
}

// UserMessage carries free-form text inputs (program output, the goal, etc).
type UserMessage struct {
	Content []TextContent
}

func (UserMessage) Role() Role { return RoleUser }
func (UserMessage) isMessage() {}

// AssistantMessage is what a provider produces in one turn. Content is an
// ordered mix of text, thinking, and tool calls; the per-content signature
// fields preserve provider-native continuity tokens (Anthropic thinking
// signatures, OpenAI reasoning IDs / encrypted_content, Google thought
// signatures) so they can be replayed verbatim on the next turn.
type AssistantMessage struct {
	Content    []AssistantContent
	StopReason StopReason
	Model      string
	Usage      Usage
	Err        string // populated when StopReason is StopError
}

func (AssistantMessage) Role() Role { return RoleAssistant }
func (AssistantMessage) isMessage() {}

// ToolResultMessage feeds a tool's output back to the model. Each result is
// keyed by the ToolCall ID emitted by the model in the previous assistant
// message.
type ToolResultMessage struct {
	ToolCallID string
	ToolName   string
	Content    []TextContent
	IsError    bool
}

func (ToolResultMessage) Role() Role { return RoleToolResult }
func (ToolResultMessage) isMessage() {}

// AssistantContent is a single piece of an AssistantMessage.
type AssistantContent interface {
	isAssistantContent()
}

// TextContent is plain assistant or user text. TextSignature carries an
// opaque per-provider identifier when one is needed for replay (currently
// only OpenAI Responses uses it; other providers leave it empty).
//
// CacheBreakpoint marks the end of a byte-stable prefix: a caching-capable
// provider (Anthropic) places a prompt-cache breakpoint after this block.
// The agent sets it on the last program-output block so the churning
// exploration tail, goal, and reinforcement that follow don't invalidate
// the cached program-output prefix. Providers that cache implicitly
// (OpenAI, Google, xAI) ignore it.
type TextContent struct {
	Text            string
	TextSignature   string
	CacheBreakpoint bool
}

func (TextContent) isAssistantContent() {}

// ThinkingContent is the model's chain-of-thought. ThinkingSignature is the
// opaque payload each provider needs to replay the thought on the next turn:
//   - Anthropic: signed thinking-block signature
//   - OpenAI Responses: reasoning item id; with the encrypted payload stored
//     after a colon (`<id>:<encrypted_content>`) when the response was created
//     with reasoning.encrypted_content included
//   - Google: opaque thought signature (base64-encoded bytes)
//
// Redacted=true indicates Anthropic redacted_thinking — the visible Thinking
// is empty and ThinkingSignature holds the opaque encrypted payload.
type ThinkingContent struct {
	Thinking          string
	ThinkingSignature string
	Redacted          bool
}

func (ThinkingContent) isAssistantContent() {}

// ToolCall is the model's request to invoke a tool.
type ToolCall struct {
	ID               string
	Name             string
	Arguments        json.RawMessage
	ThoughtSignature string // Google-only; preserved for replay
}

func (ToolCall) isAssistantContent() {}

// StopReason summarizes why the model stopped generating.
type StopReason int

const (
	StopUnknown   StopReason = iota
	StopEnd                  // model finished naturally
	StopMaxTokens            // hit max_tokens / max_output_tokens
	StopToolUse              // model wants to call tools
	StopError                // provider reported an error mid-generation
)

// Usage is best-effort token accounting. Providers fill what they report.
//
// The input buckets are DISJOINT: InputTokens counts full-price input only
// and EXCLUDES the cache buckets, so cost code can price each bucket
// independently and sum without double-counting. Providers report cache
// tokens in two different shapes — Anthropic's input_tokens already excludes
// the cache buckets (disjoint), while OpenAI/xAI/Google fold cached tokens
// into their input total (subset). Each adapter normalizes to this disjoint
// invariant at its own boundary so downstream code never has to know which
// shape the provider used.
//
// Cache writes are split by TTL because Anthropic bills them at different
// premiums (5-minute at 1.25x input, 1-hour at 2x). Providers that cache
// implicitly with no write surcharge (OpenAI/Google/xAI) leave both write
// buckets zero.
type Usage struct {
	InputTokens             int // full-price input only — EXCLUDES the cache buckets below
	OutputTokens            int
	CacheReadInputTokens    int // tokens served from prompt cache (billed at a discount)
	CacheWrite5mInputTokens int // tokens written at the 5-minute TTL (Anthropic: 1.25x input)
	CacheWrite1hInputTokens int // tokens written at the 1-hour TTL (Anthropic: 2x input)
}

// usageFromSubset normalizes a provider that reports cached tokens as a
// SUBSET of its input total (OpenAI, xAI, Google) into the disjoint-bucket
// invariant: InputTokens becomes the non-cached remainder and
// CacheReadInputTokens holds the cached count. These providers cache
// automatically with no write surcharge, so both write buckets stay 0.
// (Anthropic reports input disjoint already and copies fields directly,
// bypassing this helper.)
func usageFromSubset(totalInput, output, cachedRead int) Usage {
	return Usage{
		InputTokens:          totalInput - cachedRead,
		OutputTokens:         output,
		CacheReadInputTokens: cachedRead,
	}
}

// ToolDefinition is the schema view of a tool that providers translate into
// their native function-calling format. The agent owns the actual handler;
// the provider only needs the schema.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any // JSON Schema; each provider translates to its native shape
}

// Context is what the provider sees for one Generate call: the system
// prompt, the running conversation, and the tools the model may call.
type Context struct {
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDefinition
	Cache        CacheConfig
}

// CacheTTL selects the ephemeral lifetime of a prompt-cache breakpoint on
// providers that support explicit cache control (Anthropic). The zero value
// (CacheTTL5m) is the cheap 5-minute default.
type CacheTTL int

const (
	// CacheTTL5m is the 5-minute ephemeral TTL (Anthropic default, 1.25x
	// write cost). Suitable for prefixes that are re-hit frequently.
	CacheTTL5m CacheTTL = iota
	// CacheTTL1h is the 1-hour ephemeral TTL (2x write cost). Suitable for
	// prefixes hit less than every 5 minutes, where a 5-minute TTL would go
	// cold between hits and pay the write cost repeatedly.
	CacheTTL1h
)

// CacheConfig requests prompt-cache breakpoints from providers that support
// explicit cache control (Anthropic). The zero value disables caching;
// providers that cache implicitly (OpenAI, Google, xAI) ignore it entirely.
//
// When Enabled, the Anthropic provider places up to four breakpoints,
// ordered most-stable-first: (1) end of the tool list, (2) end of the
// system prompt, (3) the TextContent block flagged CacheBreakpoint, and
// (4) a rolling breakpoint at the end of the message history. See the
// prompt-caching section of plans/dedicated-modeling-turn.md.
type CacheConfig struct {
	Enabled bool
	// SystemTTL is the TTL for the system-prompt breakpoint (breakpoint 2).
	// Work mode passes CacheTTL5m; modeling mode passes CacheTTL1h because
	// modeling turns fire less often. The other three breakpoints always use
	// the 5-minute default — they sit on prefixes that are re-hit within a
	// cycle (tools, program output) or change every turn (rolling).
	SystemTTL CacheTTL
}

// Options are knobs the agent passes through. Each provider applies what it
// can and ignores the rest.
type Options struct {
	MaxTokens int
}

// Provider is the abstraction over Anthropic / OpenAI / Google / xAI.
// Implementations translate Context to/from their native SDK shapes,
// preserving signature fields so multi-turn reasoning round-trips.
type Provider interface {
	Name() string
	DefaultModel() string
	Generate(ctx context.Context, model string, c Context, opts Options) (AssistantMessage, error)
}

// JoinText concatenates the Text fields of content with newlines. Exposed
// for callers (renderers, tests) that operate on TextContent slices.
func JoinText(content []TextContent) string {
	if len(content) == 0 {
		return ""
	}
	if len(content) == 1 {
		return content[0].Text
	}
	var b []byte
	for i, c := range content {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, c.Text...)
	}
	return string(b)
}
