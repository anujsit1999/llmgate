package provider

import (
	"context"
	"strings"
	"time"

	"github.com/anujsit1999/llmgate/config"
)

func init() {
	// type: mock — offline provider for demos and tests; needs no credentials.
	Register("mock", func(_ context.Context, name string, _ config.Provider) (Provider, error) {
		return NewMock(name), nil
	})
}

// Mock is a deterministic, offline provider for demos, local development and
// tests. It echoes the last user message, which also makes redaction visible.
type Mock struct {
	name string
	// Delay between streamed words (zero in tests).
	Delay time.Duration
	// Fail, if set, is returned by every call (used to exercise fallback).
	Fail error
}

// NewMock returns a mock provider.
func NewMock(name string) *Mock { return &Mock{name: name, Delay: 20 * time.Millisecond} }

// Name implements Provider.
func (m *Mock) Name() string { return m.name }

func (m *Mock) reply(req Request) string {
	last := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			last = req.Messages[i].Content
			break
		}
	}
	return "mock(" + req.Model + ") received: " + last
}

func approxTokens(s string) int { return max(1, len(s)/4) }

func (m *Mock) usage(req Request, out string) Usage {
	in := approxTokens(req.System)
	for _, msg := range req.Messages {
		in += approxTokens(msg.Content)
	}
	return Usage{InputTokens: in, OutputTokens: approxTokens(out)}
}

// Complete implements Provider.
func (m *Mock) Complete(_ context.Context, req Request) (*Response, error) {
	if m.Fail != nil {
		return nil, m.Fail
	}
	out := m.reply(req)
	return &Response{ID: "mock-1", Content: out, FinishReason: "stop", Usage: m.usage(req, out)}, nil
}

// Stream implements Provider.
func (m *Mock) Stream(ctx context.Context, req Request, emit func(Chunk) error) error {
	if m.Fail != nil {
		return m.Fail
	}
	out := m.reply(req)
	words := strings.SplitAfter(out, " ")
	for _, w := range words {
		if m.Delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(m.Delay):
			}
		}
		if err := emit(Chunk{Delta: w}); err != nil {
			return err
		}
	}
	u := m.usage(req, out)
	return emit(Chunk{FinishReason: "stop", Usage: &u})
}
