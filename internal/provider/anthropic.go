package provider

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

type Anthropic struct {
	client *anthropic.Client
	model  string
}

func NewAnthropic(model string) *Anthropic {
	c := anthropic.NewClient()
	if model == "" {
		model = string(anthropic.ModelClaudeOpus4_7)
	}
	return &Anthropic{client: &c, model: model}
}

func (p *Anthropic) Name() string         { return "anthropic" }
func (p *Anthropic) DefaultModel() string { return p.model }

func (p *Anthropic) Generate(ctx context.Context, model string, c Context, opts Options) (AssistantMessage, error) {
	if model == "" {
		model = p.model
	}

	tools := make([]anthropic.ToolUnionParam, len(c.Tools))
	for i, t := range c.Tools {
		props, _ := t.Parameters["properties"].(map[string]any)
		var required []string
		if r, ok := t.Parameters["required"].([]string); ok {
			required = r
		} else if r, ok := t.Parameters["required"].([]any); ok {
			for _, v := range r {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		}
		tools[i] = anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   required,
				},
			},
		}
	}

	// Breakpoint 1: end of the tool list. Tools are identical across work and
	// modeling modes, so this prefix caches once and is reused by both.
	if c.Cache.Enabled && len(tools) > 0 {
		tools[len(tools)-1].OfTool.CacheControl = ephemeralCache(CacheTTL5m)
	}

	messages, err := buildAnthropicMessages(c.Messages, c.Cache.Enabled)
	if err != nil {
		return AssistantMessage{}, err
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Messages:  messages,
		Tools:     tools,
	}
	if c.SystemPrompt != "" {
		sys := anthropic.TextBlockParam{Text: c.SystemPrompt}
		// Breakpoint 2: end of the (mode-specific) system prompt, at the
		// caller's chosen TTL — 5m for work, 1h for modeling.
		if c.Cache.Enabled {
			sys.CacheControl = ephemeralCache(c.Cache.SystemTTL)
		}
		params.System = []anthropic.TextBlockParam{sys}
	}

	resp, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return AssistantMessage{}, err
	}

	out := AssistantMessage{Model: string(resp.Model)}
	// Anthropic already reports input_tokens disjoint from the cache buckets,
	// so the fields copy straight across with no subtraction.
	out.Usage.InputTokens = int(resp.Usage.InputTokens)
	out.Usage.OutputTokens = int(resp.Usage.OutputTokens)
	out.Usage.CacheReadInputTokens = int(resp.Usage.CacheReadInputTokens)
	// cache_creation breaks the write total down by TTL so each portion can be
	// priced at its own premium (5m at 1.25x, 1h at 2x).
	out.Usage.CacheWrite5mInputTokens = int(resp.Usage.CacheCreation.Ephemeral5mInputTokens)
	out.Usage.CacheWrite1hInputTokens = int(resp.Usage.CacheCreation.Ephemeral1hInputTokens)

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			out.Content = append(out.Content, TextContent{Text: block.Text})
		case "thinking":
			out.Content = append(out.Content, ThinkingContent{
				Thinking:          block.Thinking,
				ThinkingSignature: block.Signature,
			})
		case "redacted_thinking":
			out.Content = append(out.Content, ThinkingContent{
				Redacted:          true,
				ThinkingSignature: block.Data,
			})
		case "tool_use":
			out.Content = append(out.Content, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: block.Input,
			})
		}
	}

	switch resp.StopReason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		out.StopReason = StopEnd
	case anthropic.StopReasonMaxTokens:
		out.StopReason = StopMaxTokens
	case anthropic.StopReasonToolUse:
		out.StopReason = StopToolUse
	default:
		out.StopReason = StopEnd
	}

	return out, nil
}

// ephemeralCache builds an ephemeral cache_control value at the given TTL.
func ephemeralCache(ttl CacheTTL) anthropic.CacheControlEphemeralParam {
	cc := anthropic.NewCacheControlEphemeralParam()
	if ttl == CacheTTL1h {
		cc.TTL = anthropic.CacheControlEphemeralTTLTTL1h
	} else {
		cc.TTL = anthropic.CacheControlEphemeralTTLTTL5m
	}
	return cc
}

// setBlockCacheControl draws a cache breakpoint after b, on whichever block
// variant it holds. cache_control is rejected on thinking blocks, so those
// are skipped — when the rolling breakpoint would land on a trailing
// thinking block it simply isn't placed (the earlier breakpoints still hit).
func setBlockCacheControl(b *anthropic.ContentBlockParamUnion, ttl CacheTTL) {
	cc := ephemeralCache(ttl)
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = cc
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = cc
	}
}

// buildAnthropicMessages translates the neutral conversation into Anthropic
// MessageParams. Consecutive ToolResultMessages are coalesced into a single
// user-role message with multiple tool_result blocks, matching what the API
// expects.
//
// When cache is set, two breakpoints are placed: breakpoint 3 after any
// UserMessage block flagged CacheBreakpoint (the byte-stable program-output
// prefix), and breakpoint 4 — rolling — after the final block of the last
// message, so each accumulating tool-use turn within a cycle hits cache at
// full prior depth.
func buildAnthropicMessages(msgs []Message, cache bool) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam

	for i := 0; i < len(msgs); i++ {
		switch m := msgs[i].(type) {
		case UserMessage:
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Content))
			for _, c := range m.Content {
				b := anthropic.NewTextBlock(c.Text)
				if cache && c.CacheBreakpoint {
					b.OfText.CacheControl = ephemeralCache(CacheTTL5m)
				}
				blocks = append(blocks, b)
			}
			out = append(out, anthropic.NewUserMessage(blocks...))

		case AssistantMessage:
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Content))
			for _, c := range m.Content {
				switch ac := c.(type) {
				case TextContent:
					blocks = append(blocks, anthropic.NewTextBlock(ac.Text))
				case ThinkingContent:
					if ac.Redacted {
						blocks = append(blocks, anthropic.NewRedactedThinkingBlock(ac.ThinkingSignature))
					} else if ac.ThinkingSignature != "" {
						blocks = append(blocks, anthropic.NewThinkingBlock(ac.ThinkingSignature, ac.Thinking))
					}
					// Drop unsigned thinking — Anthropic rejects it on replay.
				case ToolCall:
					var input any
					if len(ac.Arguments) == 0 {
						input = map[string]any{}
					} else {
						input = ac.Arguments
					}
					blocks = append(blocks, anthropic.NewToolUseBlock(ac.ID, input, ac.Name))
				}
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))

		case ToolResultMessage:
			// Coalesce this and any consecutive tool-result messages.
			var blocks []anthropic.ContentBlockParamUnion
			for ; i < len(msgs); i++ {
				tr, ok := msgs[i].(ToolResultMessage)
				if !ok {
					i--
					break
				}
				body := JoinText(tr.Content)
				blocks = append(blocks, anthropic.NewToolResultBlock(tr.ToolCallID, body, tr.IsError))
			}
			out = append(out, anthropic.NewUserMessage(blocks...))

		default:
			return nil, fmt.Errorf("unsupported message type %T", m)
		}
	}

	// Breakpoint 4: rolling, at the very end of the message history. Default
	// 5-minute TTL — this line moves every turn, so an extended TTL would
	// just pay the write cost without being re-hit.
	if cache && len(out) > 0 {
		if last := out[len(out)-1].Content; len(last) > 0 {
			setBlockCacheControl(&last[len(last)-1], CacheTTL5m)
		}
	}

	return out, nil
}
