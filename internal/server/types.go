package server

import (
	"encoding/json"
	"errors"
	"strings"
)

// OpenAI-compatible wire types. Only the subset llmgate supports is modelled;
// unknown request fields are ignored, as OpenAI clients send many optional ones.

// Content accepts either a plain string or an array of {type:"text"} parts.
type Content string

// UnmarshalJSON implements json.Unmarshaler.
func (c *Content) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = Content(s)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return errors.New("content must be a string or an array of text parts")
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return errors.New("only text content parts are supported")
		}
		sb.WriteString(p.Text)
	}
	*c = Content(sb.String())
	return nil
}

// ChatMessage is one message in a chat request.
type ChatMessage struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// ChatRequest is the body of POST /v1/chat/completions.
type ChatRequest struct {
	Model               string        `json:"model"`
	Messages            []ChatMessage `json:"messages"`
	MaxTokens           *int          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int          `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
	Stream              bool          `json:"stream,omitempty"`
}

// Usage mirrors OpenAI's usage block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is a non-streaming completion.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

// ChatChoice is one completion choice.
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatChunk is one server-sent event in a streaming completion.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// ChunkChoice is the delta for one choice.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is incremental message content.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ErrorBody is OpenAI's error envelope.
type ErrorBody struct {
	Error APIError `json:"error"`
}

// APIError describes a failure without leaking upstream internals.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
