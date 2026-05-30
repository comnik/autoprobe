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

	messages, err := buildAnthropicMessages(c.Messages)
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
		params.System = []anthropic.TextBlockParam{{Text: c.SystemPrompt}}
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
	out.Usage.CacheWriteInputTokens = int(resp.Usage.CacheCreationInputTokens)

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

// buildAnthropicMessages translates the neutral conversation into Anthropic
// MessageParams. Consecutive ToolResultMessages are coalesced into a single
// user-role message with multiple tool_result blocks, matching what the API
// expects.
func buildAnthropicMessages(msgs []Message) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam

	for i := 0; i < len(msgs); i++ {
		switch m := msgs[i].(type) {
		case UserMessage:
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(m.Content))
			for _, c := range m.Content {
				blocks = append(blocks, anthropic.NewTextBlock(c.Text))
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

	return out, nil
}
