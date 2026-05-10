package main

import (
	"context"
	"encoding/json"
)

// Role identifies which side of the conversation a message belongs to.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
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
type TextContent struct {
	Text          string
	TextSignature string
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
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Context is what the provider sees for one Generate call: the system
// prompt, the running conversation, and the tools the model may call.
type Context struct {
	SystemPrompt string
	Messages     []Message
	Tools        []ToolDefinition
}

// Options are knobs the agent passes through. Each provider applies what it
// can and ignores the rest.
type Options struct {
	MaxTokens int
}

// Provider is the abstraction over Anthropic / OpenAI / Google. Provider
// implementations translate Context to/from their native SDK shapes,
// preserving signature fields so multi-turn reasoning round-trips.
type Provider interface {
	Name() string
	DefaultModel() string
	Generate(ctx context.Context, model string, c Context, opts Options) (AssistantMessage, error)
}
