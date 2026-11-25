package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/anujsit1999/llmgate/config"
)

// The OpenAI Chat Completions API has become the industry's lingua franca, so
// one adapter covers many vendors. Each registered type below is just a set
// of defaults (base URL, auth header) over the same code.
func init() {
	// type: openai — api.openai.com.
	Register("openai", openAIFactory(openAIOptions{defaultBaseURL: "https://api.openai.com/v1", needKey: true}))

	// type: azure_openai — Azure OpenAI / Azure AI Foundry v1 API.
	// base_url: https://<resource>.openai.azure.com/openai/v1
	Register("azure_openai", openAIFactory(openAIOptions{authHeader: "api-key", needBaseURL: true, needKey: true}))

	// type: ollama — local models (Llama, Mistral, Qwen, ...) via Ollama.
	Register("ollama", openAIFactory(openAIOptions{defaultBaseURL: "http://localhost:11434/v1"}))

	// type: openai_compatible — anything else that speaks the protocol:
	// vLLM, LM Studio, Groq, Together, Fireworks, OpenRouter, Google Gemini's
	// OpenAI endpoint, Bedrock's bedrock-mantle endpoint (Llama, gpt-oss), ...
	Register("openai_compatible", openAIFactory(openAIOptions{needBaseURL: true}))
}

type openAIOptions struct {
	defaultBaseURL string
	authHeader     string // "" means "Authorization: Bearer <key>"
	needBaseURL    bool
	needKey        bool
}

func openAIFactory(o openAIOptions) Factory {
	return func(_ context.Context, name string, cfg config.Provider) (Provider, error) {
		base := cfg.BaseURL
		if base == "" {
			base = o.defaultBaseURL
		}
		if base == "" && o.needBaseURL {
			return nil, errors.New("base_url is required")
		}
		key := cfg.APIKey()
		if key == "" && o.needKey {
			return nil, fmt.Errorf("api key missing: set the %q environment variable", cfg.APIKeyEnv)
		}
		p := NewOpenAI(name, base, key, &http.Client{})
		p.authHeader = o.authHeader
		p.headers = cfg.Headers
		return p, nil
	}
}

// OpenAI talks to any OpenAI-compatible Chat Completions API.
type OpenAI struct {
	name       string
	baseURL    string
	apiKey     string
	authHeader string            // custom header name for the key (Azure: "api-key")
	headers    map[string]string // extra static headers from config
	http       *http.Client
}

// NewOpenAI builds an adapter. baseURL should include the version path, e.g.
// https://api.openai.com/v1 or http://localhost:11434/v1.
func NewOpenAI(name, baseURL, apiKey string, client *http.Client) *OpenAI {
	if client == nil {
		client = &http.Client{}
	}
	return &OpenAI{name: name, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: client}
}

// Name implements Provider.
func (o *OpenAI) Name() string { return o.name }

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaRequest struct {
	Model           string         `json:"model"`
	Messages        []oaMessage    `json:"messages"`
	MaxTokens       int            `json:"max_completion_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   map[string]any `json:"stream_options,omitempty"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Message      oaMessage `json:"message"`
		Delta        oaMessage `json:"delta"`
		FinishReason *string   `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

func (o *OpenAI) body(req Request, stream bool) oaRequest {
	msgs := make([]oaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, oaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, oaMessage{Role: m.Role, Content: m.Content})
	}
	b := oaRequest{Model: req.Model, Messages: msgs, MaxTokens: req.MaxTokens, Temperature: req.Temperature, Stream: stream}
	// The protocol accepts low/medium/high; clamp the higher levels.
	switch req.Effort {
	case "low", "medium", "high":
		b.ReasoningEffort = req.Effort
	case "xhigh", "max":
		b.ReasoningEffort = "high"
	}
	if stream {
		b.StreamOptions = map[string]any{"include_usage": true}
	}
	return b
}

// encode marshals the standard body, then merges extra params into it.
func encode(body oaRequest, extra map[string]any) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil || len(extra) == 0 {
		return buf, err
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		setPath(m, strings.Split(k, "."), v)
	}
	return json.Marshal(m)
}

// setPath sets m[a][b][c] = v for path [a b c], creating maps as needed.
func setPath(m map[string]any, path []string, v any) {
	for _, p := range path[:len(path)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[path[len(path)-1]] = v
}

func (o *OpenAI) do(ctx context.Context, req Request, stream bool) (*http.Response, error) {
	buf, err := encode(o.body(req, stream), req.Extra)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range o.headers {
		httpReq.Header.Set(k, v)
	}
	if o.apiKey != "" {
		if o.authHeader != "" {
			httpReq.Header.Set(o.authHeader, o.apiKey)
		} else {
			httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)
		}
	}
	resp, err := o.http.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, &Error{Provider: o.name, Retryable: true, Err: err}
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		// Keep a bounded snippet for server logs; it is never echoed to clients.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, newStatusError(o.name, resp.StatusCode, fmt.Errorf("%s", bytes.TrimSpace(snippet)))
	}
	return resp, nil
}

// Complete implements Provider.
func (o *OpenAI) Complete(ctx context.Context, req Request) (*Response, error) {
	resp, err := o.do(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out oaResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, &Error{Provider: o.name, Status: resp.StatusCode, Retryable: true, Err: fmt.Errorf("decode: %w", err)}
	}
	if len(out.Choices) == 0 {
		return nil, &Error{Provider: o.name, Status: resp.StatusCode, Retryable: true, Err: errors.New("no choices in response")}
	}
	r := &Response{ID: out.ID, Content: out.Choices[0].Message.Content, FinishReason: "stop"}
	if fr := out.Choices[0].FinishReason; fr != nil && *fr != "" {
		r.FinishReason = *fr
	}
	if out.Usage != nil {
		r.Usage = Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	}
	return r, nil
}

// Stream implements Provider.
func (o *OpenAI) Stream(ctx context.Context, req Request, emit func(Chunk) error) error {
	resp, err := o.do(ctx, req, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var finish string
	var usage *Usage
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue // comments, event: lines, keep-alives
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var ev oaResponse
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return &Error{Provider: o.name, Status: resp.StatusCode, Err: fmt.Errorf("decode stream: %w", err)}
		}
		if ev.Usage != nil {
			usage = &Usage{InputTokens: ev.Usage.PromptTokens, OutputTokens: ev.Usage.CompletionTokens}
		}
		for _, c := range ev.Choices {
			if c.Delta.Content != "" {
				if err := emit(Chunk{Delta: c.Delta.Content}); err != nil {
					return err
				}
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return &Error{Provider: o.name, Status: resp.StatusCode, Err: err}
	}
	if finish == "" {
		finish = "stop"
	}
	if usage == nil {
		usage = &Usage{}
	}
	return emit(Chunk{FinishReason: finish, Usage: usage})
}
