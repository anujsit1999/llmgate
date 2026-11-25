package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anujsit1999/llmgate/config"
)

func TestRegistryHasBuiltins(t *testing.T) {
	got := strings.Join(Types(), ",")
	for _, want := range []string{"anthropic", "azure_openai", "bedrock", "mock", "ollama", "openai", "openai_compatible"} {
		if !strings.Contains(got, want) {
			t.Errorf("type %q not registered (have %s)", want, got)
		}
	}
	if _, err := Build(context.Background(), "x", config.Provider{Type: "nope"}); err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestOpenAIComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("auth header = %q", got)
		}
		var body oaRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Messages[0].Role != "system" || body.MaxTokens != 50 {
			t.Errorf("unexpected body %+v", body)
		}
		fmt.Fprint(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	p := NewOpenAI("oa", srv.URL+"/v1", "secret", srv.Client())
	resp, err := p.Complete(context.Background(), Request{
		Model: "m", System: "be nice", MaxTokens: 50, Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hi" || resp.Usage.InputTokens != 3 || resp.FinishReason != "stop" {
		t.Fatalf("unexpected response %+v", resp)
	}
}

func TestAzureUsesAPIKeyHeader(t *testing.T) {
	t.Setenv("TEST_AZURE_KEY", "az-secret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "az-secret" || r.Header.Get("Authorization") != "" {
			t.Errorf("azure auth headers wrong: %v", r.Header)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	p, err := Build(context.Background(), "az", config.Provider{Type: "azure_openai", BaseURL: srv.URL, APIKeyEnv: "TEST_AZURE_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Complete(context.Background(), Request{Model: "gpt", Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, line := range []string{
			`{"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
			`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":2}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewOpenAI("oa", srv.URL, "", srv.Client())
	var text strings.Builder
	var last Chunk
	err := p.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}}, func(c Chunk) error {
		text.WriteString(c.Delta)
		last = c
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "Hello" || last.FinishReason != "stop" || last.Usage == nil || last.Usage.OutputTokens != 2 {
		t.Fatalf("text=%q last=%+v", text.String(), last)
	}
}

func TestOpenAIErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{{429, true}, {500, true}, {503, true}, {401, true}, {404, true}, {400, false}, {422, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"x"}`, tc.status)
		}))
		p := NewOpenAI("oa", srv.URL, "", srv.Client())
		_, err := p.Complete(context.Background(), Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
		if IsRetryable(err) != tc.retryable {
			t.Errorf("status %d: retryable = %v, want %v", tc.status, IsRetryable(err), tc.retryable)
		}
		srv.Close()
	}
}

func TestAnthropicFinishReason(t *testing.T) {
	cases := map[string]string{"end_turn": "stop", "max_tokens": "length", "refusal": "content_filter", "tool_use": "tool_calls"}
	for in, want := range cases {
		if got := anthropicFinishReason(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

func TestEncodeMergesExtraParams(t *testing.T) {
	buf, err := encode(oaRequest{Model: "m"}, map[string]any{"top_p": 0.5, "options.num_ctx": 8192})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(buf, &m)
	opts, _ := m["options"].(map[string]any)
	if m["model"] != "m" || m["top_p"] != 0.5 || opts["num_ctx"] != float64(8192) {
		t.Fatalf("merged body wrong: %s", buf)
	}
}
