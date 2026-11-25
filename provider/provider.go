// Package provider defines the upstream-LLM abstraction and its adapters.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// Message is one conversational turn. Role is "user" or "assistant";
// system prompts travel separately in Request.System.
type Message struct {
	Role    string
	Content string
}

// Request is a provider-neutral chat request.
type Request struct {
	Model       string // upstream model id
	System      string
	Messages    []Message
	MaxTokens   int
	Temperature *float64 // honoured only by providers that accept it
	Effort      string   // "", low, medium, high, xhigh, max
	// ServerSideFallback enables Anthropic's server-side refusal fallback.
	ServerSideFallback bool
	// Extra holds additional provider-specific body fields: allowed client
	// params merged with the target's configured params. Keys are already
	// validated (config.CheckParamKey); dotted keys address nested fields.
	Extra map[string]any
}

// Usage is token accounting for one request.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is a completed (non-streaming) result.
type Response struct {
	ID           string
	Content      string
	FinishReason string // OpenAI vocabulary: stop, length, content_filter, tool_calls
	Usage        Usage
}

// Chunk is one streaming increment. The final chunk carries FinishReason and Usage.
type Chunk struct {
	Delta        string
	FinishReason string
	Usage        *Usage
}

// Provider is implemented by every upstream adapter.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
	// Stream calls emit for each chunk. If emit returns an error, streaming stops.
	Stream(ctx context.Context, req Request, emit func(Chunk) error) error
}

// Error is an upstream failure with enough context to decide on fallback.
type Error struct {
	Provider  string
	Status    int // upstream HTTP status, 0 for network errors
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: upstream status %d: %v", e.Provider, e.Status, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether a different target might succeed.
func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// newStatusError classifies an upstream HTTP status.
//
// Only statuses caused by the request itself (400, 413, 422) are final:
// another deployment would reject the same request. Everything else — 429,
// 5xx/529, but also 401/403/404 (a misconfigured or missing deployment) — is
// a deployment problem, so it counts against the circuit breaker and the
// router falls back.
func newStatusError(provider string, status int, err error) *Error {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return &Error{Provider: provider, Status: status, Retryable: false, Err: err}
	}
	return &Error{Provider: provider, Status: status, Retryable: true, Err: err}
}
