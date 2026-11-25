package provider

import (
	"context"
	"errors"

	"github.com/anthropics/anthropic-sdk-go/v2/bedrock"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/anujsit1999/llmgate/config"
)

func init() {
	// type: bedrock — Claude on Amazon Bedrock via the Mantle (Messages API)
	// endpoint. Model ids take an "anthropic." prefix, e.g.
	// "anthropic.claude-opus-5-5".
	//
	// Non-Claude models on Bedrock (Llama, gpt-oss, ...) are served through
	// Bedrock's OpenAI-compatible endpoint instead: use type
	// openai_compatible with base_url https://bedrock-mantle.<region>.api.aws/v1.
	Register("bedrock", func(ctx context.Context, name string, cfg config.Provider) (Provider, error) {
		if cfg.Region == "" {
			return nil, errors.New("region is required")
		}
		// With no API key the SDK falls back to the standard AWS credential
		// chain (env vars, shared profile, IAM role) — preferred in production.
		client, err := bedrock.NewMantleClient(ctx, bedrock.MantleClientConfig{
			APIKey:    cfg.APIKey(),
			AWSRegion: cfg.Region,
			BaseURL:   cfg.BaseURL,
		}, option.WithMaxRetries(2))
		if err != nil {
			return nil, err
		}
		return &Anthropic{name: name, msgs: client.Beta.Messages}, nil
	})
}
