package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"google.golang.org/genai"
)

// Google talks to Gemini via google.golang.org/genai. The neutral
// ToolCall.ThoughtSignature carries Gemini's per-call thoughtSignature
// (base64-encoded so it round-trips through a string), letting Gemini reuse
// its thought context across turns.
type Google struct {
	client *genai.Client
	model  string
}

func NewGoogle(model string) (*Google, error) {
	apiKey := envLookup("GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = envLookup("GOOGLE_API_KEY")
	}
	cfg := &genai.ClientConfig{APIKey: apiKey, Backend: genai.BackendGeminiAPI}
	c, err := genai.NewClient(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	if model == "" {
		model = "gemini-2.5-pro"
	}
	return &Google{client: c, model: model}, nil
}

func (p *Google) Name() string         { return "google" }
func (p *Google) DefaultModel() string { return p.model }

func (p *Google) Generate(ctx context.Context, model string, c Context, opts Options) (AssistantMessage, error) {
	if model == "" {
		model = p.model
	}

	declarations := make([]*genai.FunctionDeclaration, 0, len(c.Tools))
	for _, t := range c.Tools {
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:                 t.Name,
			Description:          t.Description,
			ParametersJsonSchema: t.Parameters,
		})
	}

	contents, err := buildGoogleContents(c.Messages)
	if err != nil {
		return AssistantMessage{}, err
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}

	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(maxTokens),
		ThinkingConfig:  &genai.ThinkingConfig{IncludeThoughts: true},
	}
	if len(declarations) > 0 {
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: declarations}}
	}
	if c.SystemPrompt != "" {
		cfg.SystemInstruction = &genai.Content{
			Role:  string(genai.RoleUser),
			Parts: []*genai.Part{{Text: c.SystemPrompt}},
		}
	}

	resp, err := p.client.Models.GenerateContent(ctx, model, contents, cfg)
	if err != nil {
		return AssistantMessage{}, err
	}

	out := AssistantMessage{Model: resp.ModelVersion}
	if resp.UsageMetadata != nil {
		// prompt_token_count includes cached_content_token_count (subset);
		// usageFromSubset normalizes to the disjoint invariant. Google also
		// bills cache storage per-token-per-hour, which can't be derived from a
		// single response, so cache write stays 0 (a documented undercount).
		out.Usage = usageFromSubset(
			int(resp.UsageMetadata.PromptTokenCount),
			int(resp.UsageMetadata.CandidatesTokenCount),
			int(resp.UsageMetadata.CachedContentTokenCount),
		)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		out.StopReason = StopEnd
		return out, nil
	}
	cand := resp.Candidates[0]

	for _, part := range cand.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			args, _ := json.Marshal(part.FunctionCall.Args)
			out.Content = append(out.Content, ToolCall{
				ID:               part.FunctionCall.ID,
				Name:             part.FunctionCall.Name,
				Arguments:        args,
				ThoughtSignature: base64.StdEncoding.EncodeToString(part.ThoughtSignature),
			})
		case part.Thought:
			out.Content = append(out.Content, ThinkingContent{
				Thinking:          part.Text,
				ThinkingSignature: base64.StdEncoding.EncodeToString(part.ThoughtSignature),
			})
		case part.Text != "":
			out.Content = append(out.Content, TextContent{Text: part.Text})
		}
	}

	switch cand.FinishReason {
	case genai.FinishReasonStop, "":
		out.StopReason = StopEnd
		for _, c := range out.Content {
			if _, ok := c.(ToolCall); ok {
				out.StopReason = StopToolUse
				break
			}
		}
	case genai.FinishReasonMaxTokens:
		out.StopReason = StopMaxTokens
	default:
		out.StopReason = StopError
		if cand.FinishMessage != "" {
			out.Err = cand.FinishMessage
		} else {
			out.Err = string(cand.FinishReason)
		}
	}

	return out, nil
}

// buildGoogleContents translates the neutral conversation into Gemini's
// Content list. Tool results are emitted as user-role messages with
// functionResponse parts. Consecutive tool-result messages are coalesced
// into a single Content per Gemini's expectations.
func buildGoogleContents(msgs []Message) ([]*genai.Content, error) {
	var out []*genai.Content

	for i := 0; i < len(msgs); i++ {
		switch m := msgs[i].(type) {
		case UserMessage:
			parts := make([]*genai.Part, 0, len(m.Content))
			for _, c := range m.Content {
				if c.Text == "" {
					continue
				}
				parts = append(parts, &genai.Part{Text: c.Text})
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, &genai.Content{Role: string(genai.RoleUser), Parts: parts})

		case AssistantMessage:
			parts := make([]*genai.Part, 0, len(m.Content))
			for _, c := range m.Content {
				switch c := c.(type) {
				case TextContent:
					if c.Text == "" {
						continue
					}
					parts = append(parts, &genai.Part{Text: c.Text})
				case ThinkingContent:
					sig, _ := base64.StdEncoding.DecodeString(c.ThinkingSignature)
					if len(sig) == 0 && c.Thinking == "" {
						continue
					}
					parts = append(parts, &genai.Part{
						Text:             c.Thinking,
						Thought:          true,
						ThoughtSignature: sig,
					})
				case ToolCall:
					var args map[string]any
					if len(c.Arguments) > 0 {
						_ = json.Unmarshal(c.Arguments, &args)
					}
					sig, _ := base64.StdEncoding.DecodeString(c.ThoughtSignature)
					parts = append(parts, &genai.Part{
						FunctionCall: &genai.FunctionCall{
							ID:   c.ID,
							Name: c.Name,
							Args: args,
						},
						ThoughtSignature: sig,
					})
				}
			}
			if len(parts) == 0 {
				continue
			}
			out = append(out, &genai.Content{Role: string(genai.RoleModel), Parts: parts})

		case ToolResultMessage:
			parts := []*genai.Part{}
			for ; i < len(msgs); i++ {
				tr, ok := msgs[i].(ToolResultMessage)
				if !ok {
					i--
					break
				}
				parts = append(parts, &genai.Part{
					FunctionResponse: &genai.FunctionResponse{
						ID:       tr.ToolCallID,
						Name:     tr.ToolName,
						Response: map[string]any{"output": JoinText(tr.Content), "isError": tr.IsError},
					},
				})
			}
			out = append(out, &genai.Content{Role: string(genai.RoleUser), Parts: parts})

		default:
			return nil, fmt.Errorf("unsupported message type %T", m)
		}
	}

	return out, nil
}
