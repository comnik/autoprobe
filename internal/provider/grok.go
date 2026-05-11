package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

const grokBaseURL = "https://api.x.ai/v1"

// Grok talks to xAI via the OpenAI-compatible Chat Completions endpoint at
// https://api.x.ai/v1. xAI's reasoning_content is not signed for replay, so
// reasoning is dropped between turns — the agent loop tolerates this the
// same way it does for non-extended-thinking Anthropic.
type Grok struct {
	client *openai.Client
	model  string
}

func NewGrok(model string) *Grok {
	c := openai.NewClient(
		option.WithAPIKey(getGrokKey()),
		option.WithBaseURL(grokBaseURL),
	)
	if model == "" {
		model = "grok-4"
	}
	return &Grok{client: &c, model: model}
}

func getGrokKey() string {
	for _, k := range []string{"XAI_API_KEY", "GROK_API_KEY"} {
		if v := envLookup(k); v != "" {
			return v
		}
	}
	return ""
}

func (p *Grok) Name() string         { return "grok" }
func (p *Grok) DefaultModel() string { return p.model }

func (p *Grok) Generate(ctx context.Context, model string, c Context, opts Options) (AssistantMessage, error) {
	if model == "" {
		model = p.model
	}

	tools := make([]openai.ChatCompletionToolParam, 0, len(c.Tools))
	for _, t := range c.Tools {
		tools = append(tools, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  t.Parameters,
			},
		})
	}

	messages, err := buildGrokMessages(c.SystemPrompt, c.Messages)
	if err != nil {
		return AssistantMessage{}, err
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(model),
		Messages:            messages,
		MaxCompletionTokens: param.NewOpt(int64(maxTokens)),
		Tools:               tools,
	}

	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return AssistantMessage{}, err
	}

	out := AssistantMessage{Model: resp.Model}
	out.Usage.InputTokens = int(resp.Usage.PromptTokens)
	out.Usage.OutputTokens = int(resp.Usage.CompletionTokens)

	if len(resp.Choices) == 0 {
		out.StopReason = StopError
		out.Err = "no choices in response"
		return out, nil
	}
	choice := resp.Choices[0]

	if text := choice.Message.Content; text != "" {
		out.Content = append(out.Content, TextContent{Text: text})
	}
	for _, tc := range choice.Message.ToolCalls {
		out.Content = append(out.Content, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: []byte(tc.Function.Arguments),
		})
	}

	switch choice.FinishReason {
	case "stop":
		out.StopReason = StopEnd
	case "length":
		out.StopReason = StopMaxTokens
	case "tool_calls", "function_call":
		out.StopReason = StopToolUse
	case "content_filter":
		out.StopReason = StopError
		out.Err = "content filtered"
	default:
		out.StopReason = StopEnd
	}
	// xAI sometimes reports finish_reason=stop alongside tool calls.
	if out.StopReason == StopEnd && len(choice.Message.ToolCalls) > 0 {
		out.StopReason = StopToolUse
	}

	return out, nil
}

// buildGrokMessages translates the neutral conversation into Chat Completions
// messages. Thinking blocks are dropped because xAI does not provide a
// replayable signature.
func buildGrokMessages(systemPrompt string, msgs []Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	var out []openai.ChatCompletionMessageParamUnion
	if systemPrompt != "" {
		out = append(out, openai.SystemMessage(systemPrompt))
	}

	for _, m := range msgs {
		switch m := m.(type) {
		case UserMessage:
			text := JoinText(m.Content)
			if text == "" {
				continue
			}
			out = append(out, openai.UserMessage(text))

		case AssistantMessage:
			var text strings.Builder
			var calls []openai.ChatCompletionMessageToolCallParam
			for _, c := range m.Content {
				switch c := c.(type) {
				case TextContent:
					if text.Len() > 0 && c.Text != "" {
						text.WriteByte('\n')
					}
					text.WriteString(c.Text)
				case ToolCall:
					args := string(c.Arguments)
					if args == "" {
						args = "{}"
					}
					calls = append(calls, openai.ChatCompletionMessageToolCallParam{
						ID: c.ID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      c.Name,
							Arguments: args,
						},
					})
				}
			}
			if text.Len() == 0 && len(calls) == 0 {
				continue
			}
			asst := openai.ChatCompletionAssistantMessageParam{}
			if text.Len() > 0 {
				asst.Content.OfString = param.NewOpt(text.String())
			}
			if len(calls) > 0 {
				asst.ToolCalls = calls
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})

		case ToolResultMessage:
			out = append(out, openai.ToolMessage(JoinText(m.Content), m.ToolCallID))

		default:
			return nil, fmt.Errorf("unsupported message type %T", m)
		}
	}

	return out, nil
}
