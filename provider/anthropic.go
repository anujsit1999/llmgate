package provider

import (
	"context"
	"errors"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/anujsit1999/llmgate/config"
)

func init() {
	// type: anthropic — Claude via the Anthropic API.
	Register("anthropic", func(_ context.Context, name string, cfg config.Provider) (Provider, error) {
		return NewAnthropic(name, cfg.APIKey(), cfg.BaseURL), nil
	})
}

// Anthropic speaks the Claude Messages API through the official Go SDK.
// The same adapter serves Claude on the Anthropic API and on Amazon Bedrock
// (see bedrock.go) because both expose the same Messages service.
//
// It uses the beta Messages endpoint so that per-route server-side refusal
// fallbacks can be enabled; everything else behaves like the stable endpoint.
type Anthropic struct {
	name string
	msgs anthropic.BetaMessageService
}

// NewAnthropic builds an adapter for the Anthropic API. An empty apiKey lets
// the SDK resolve credentials itself (ANTHROPIC_API_KEY, `ant auth login`
// profile, workload identity federation).
func NewAnthropic(name, apiKey, baseURL string) *Anthropic {
	opts := []option.RequestOption{option.WithMaxRetries(2)}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	client := anthropic.NewClient(opts...)
	return &Anthropic{name: name, msgs: client.Beta.Messages}
}

// Name implements Provider.
func (a *Anthropic) Name() string { return a.name }

func (a *Anthropic) params(req Request) anthropic.BetaMessageNewParams {
	msgs := make([]anthropic.BetaMessageParam, 0, len(req.Messages))
	for _, m := range req.Messages {
		block := anthropic.NewBetaTextBlock(m.Content)
		if m.Role == "assistant" {
			msgs = append(msgs, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{
				Content: []anthropic.BetaContentBlockParamUnion{block},
			}})
			continue
		}
		msgs = append(msgs, anthropic.NewBetaUserMessage(block))
	}
	p := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  msgs,
	}
	if req.System != "" {
		p.System = []anthropic.BetaTextBlockParam{{Text: req.System}}
	}
	// Current Claude models reject sampling parameters (temperature/top_p) and
	// use adaptive thinking by default, so temperature is intentionally dropped.
	// Depth/cost is controlled with effort instead.
	if req.Effort != "" {
		p.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(req.Effort)}
	}
	if req.ServerSideFallback {
		p.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		p.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
	}
	return p
}

// Complete implements Provider.
func (a *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	msg, err := a.msgs.New(ctx, a.params(req), extraOptions(req.Extra)...)
	if err != nil {
		return nil, a.wrap(err)
	}
	var sb strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return &Response{
		ID:           msg.ID,
		Content:      sb.String(),
		FinishReason: anthropicFinishReason(string(msg.StopReason)),
		Usage:        Usage{InputTokens: int(msg.Usage.InputTokens), OutputTokens: int(msg.Usage.OutputTokens)},
	}, nil
}

// Stream implements Provider.
func (a *Anthropic) Stream(ctx context.Context, req Request, emit func(Chunk) error) error {
	stream := a.msgs.NewStreaming(ctx, a.params(req), extraOptions(req.Extra)...)
	defer stream.Close()

	var usage Usage
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case anthropic.BetaRawMessageStartEvent:
			usage.InputTokens = int(ev.Message.Usage.InputTokens)
		case anthropic.BetaRawContentBlockDeltaEvent:
			if td, ok := ev.Delta.AsAny().(anthropic.BetaTextDelta); ok && td.Text != "" {
				if err := emit(Chunk{Delta: td.Text}); err != nil {
					return err
				}
			}
		case anthropic.BetaRawMessageDeltaEvent:
			usage.OutputTokens = int(ev.Usage.OutputTokens)
			if ev.Delta.StopReason != "" {
				u := usage
				if err := emit(Chunk{FinishReason: anthropicFinishReason(string(ev.Delta.StopReason)), Usage: &u}); err != nil {
					return err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return a.wrap(err)
	}
	return nil
}

// extraOptions turns extra params into SDK body overrides. WithJSONSet uses
// dotted paths, so "thinking.display" sets a nested field.
func extraOptions(extra map[string]any) []option.RequestOption {
	opts := make([]option.RequestOption, 0, len(extra))
	for k, v := range extra {
		opts = append(opts, option.WithJSONSet(k, v))
	}
	return opts
}

func (a *Anthropic) wrap(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return newStatusError(a.name, apiErr.StatusCode, err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Connection-level failure: another target may well succeed.
	return &Error{Provider: a.name, Retryable: true, Err: err}
}

// anthropicFinishReason maps Anthropic stop reasons onto the gateway's
// common (OpenAI) vocabulary.
func anthropicFinishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}
