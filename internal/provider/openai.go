package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
	"github.com/openai/openai-go/shared/constant"
)

// OpenAI talks to OpenAI's Responses API. Reasoning round-trip is achieved
// by sending Include=["reasoning.encrypted_content"] on every call and
// replaying each reasoning item with its id, summary, and encrypted_content
// on the next turn. The neutral ThinkingSignature carries
// "<id>|<encrypted_content>".
type OpenAI struct {
	client *openai.Client
	model  string
}

func NewOpenAI(model string) *OpenAI {
	c := openai.NewClient(option.WithAPIKey(getOpenAIKey()))
	if model == "" {
		model = "gpt-5.3-codex"
	}
	return &OpenAI{client: &c, model: model}
}

func getOpenAIKey() string {
	for _, k := range []string{"OPENAI_API_KEY"} {
		if v := envLookup(k); v != "" {
			return v
		}
	}
	return ""
}

func (p *OpenAI) Name() string         { return "openai" }
func (p *OpenAI) DefaultModel() string { return p.model }

func (p *OpenAI) Generate(ctx context.Context, model string, c Context, opts Options) (AssistantMessage, error) {
	if model == "" {
		model = p.model
	}

	tools := make([]responses.ToolUnionParam, 0, len(c.Tools))
	for _, t := range c.Tools {
		tools = append(tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        t.Name,
				Description: param.NewOpt(t.Description),
				Parameters:  t.Parameters,
				Strict:      param.NewOpt(false),
			},
		})
	}

	input, err := buildOpenAIInput(c.Messages)
	if err != nil {
		return AssistantMessage{}, err
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	params := responses.ResponseNewParams{
		Model:           shared.ResponsesModel(model),
		Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		MaxOutputTokens: param.NewOpt(int64(maxTokens)),
		Store:           param.NewOpt(false),
		Include:         []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent},
		Tools:           tools,
		Reasoning: shared.ReasoningParam{
			Summary: shared.ReasoningSummaryAuto,
		},
	}
	if c.SystemPrompt != "" {
		params.Instructions = param.NewOpt(c.SystemPrompt)
	}

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		return AssistantMessage{}, err
	}

	out := AssistantMessage{Model: string(resp.Model)}
	// The Responses API folds cached tokens into input_tokens (subset);
	// usageFromSubset normalizes to the disjoint-bucket invariant.
	out.Usage = usageFromSubset(
		int(resp.Usage.InputTokens),
		int(resp.Usage.OutputTokens),
		int(resp.Usage.InputTokensDetails.CachedTokens),
	)

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					out.Content = append(out.Content, TextContent{
						Text:          c.Text,
						TextSignature: item.ID,
					})
				}
			}
		case "reasoning":
			summary := ""
			if len(item.Summary) > 0 {
				parts := make([]string, 0, len(item.Summary))
				for _, s := range item.Summary {
					parts = append(parts, s.Text)
				}
				summary = strings.Join(parts, "\n")
			}
			out.Content = append(out.Content, ThinkingContent{
				Thinking:          summary,
				ThinkingSignature: encodeReasoningSignature(item.ID, item.EncryptedContent),
			})
		case "function_call":
			out.Content = append(out.Content, ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: []byte(item.Arguments),
			})
		}
	}

	switch resp.Status {
	case responses.ResponseStatusCompleted:
		out.StopReason = StopEnd
		for _, item := range resp.Output {
			if item.Type == "function_call" {
				out.StopReason = StopToolUse
				break
			}
		}
	case responses.ResponseStatusIncomplete:
		// e.g., max_output_tokens
		out.StopReason = StopMaxTokens
		if resp.IncompleteDetails.Reason != "max_output_tokens" {
			out.StopReason = StopError
			out.Err = fmt.Sprintf("incomplete: %s", resp.IncompleteDetails.Reason)
		}
	case responses.ResponseStatusFailed, responses.ResponseStatusCancelled:
		out.StopReason = StopError
		out.Err = resp.Error.Message
	default:
		out.StopReason = StopEnd
	}

	return out, nil
}

func encodeReasoningSignature(id, encrypted string) string {
	if id == "" && encrypted == "" {
		return ""
	}
	return id + "|" + encrypted
}

func decodeReasoningSignature(sig string) (id, encrypted string) {
	idx := strings.IndexByte(sig, '|')
	if idx < 0 {
		return sig, ""
	}
	return sig[:idx], sig[idx+1:]
}

// buildOpenAIInput translates the neutral conversation into a flat list of
// Responses-API input items. Order matters and reasoning items must precede
// the function_calls they motivated.
func buildOpenAIInput(msgs []Message) (responses.ResponseInputParam, error) {
	var out responses.ResponseInputParam

	for _, m := range msgs {
		switch m := m.(type) {
		case UserMessage:
			text := JoinText(m.Content)
			if text == "" {
				continue
			}
			out = append(out, responses.ResponseInputItemParamOfMessage(text, responses.EasyInputMessageRoleUser))

		case AssistantMessage:
			for _, c := range m.Content {
				switch c := c.(type) {
				case TextContent:
					if strings.TrimSpace(c.Text) == "" {
						continue
					}
					out = append(out, responses.ResponseInputItemParamOfMessage(c.Text, responses.EasyInputMessageRoleAssistant))
				case ThinkingContent:
					id, encrypted := decodeReasoningSignature(c.ThinkingSignature)
					if id == "" && encrypted == "" {
						continue
					}
					reasoning := &responses.ResponseReasoningItemParam{
						ID:   id,
						Type: constant.Reasoning("reasoning"),
					}
					if c.Thinking != "" {
						reasoning.Summary = []responses.ResponseReasoningItemSummaryParam{
							{Text: c.Thinking, Type: constant.SummaryText("summary_text")},
						}
					}
					if encrypted != "" {
						reasoning.EncryptedContent = param.NewOpt(encrypted)
					}
					out = append(out, responses.ResponseInputItemUnionParam{OfReasoning: reasoning})
				case ToolCall:
					out = append(out, responses.ResponseInputItemParamOfFunctionCall(string(c.Arguments), c.ID, c.Name))
				}
			}

		case ToolResultMessage:
			out = append(out, responses.ResponseInputItemParamOfFunctionCallOutput(m.ToolCallID, JoinText(m.Content)))

		default:
			return nil, fmt.Errorf("unsupported message type %T", m)
		}
	}

	return out, nil
}
