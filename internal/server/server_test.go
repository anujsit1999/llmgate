package server

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anujsit1999/llmgate/auth"
	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/internal/metrics"
	"github.com/anujsit1999/llmgate/provider"
	"github.com/anujsit1999/llmgate/ratelimit"
	"github.com/anujsit1999/llmgate/redact"
	"github.com/anujsit1999/llmgate/router"
	"github.com/anujsit1999/llmgate/state"
)

type fixture struct {
	srv        *httptest.Server
	key        string
	limitedKey string
}

func setup(t *testing.T) fixture {
	t.Helper()
	key, hash, _ := auth.Generate()
	limited, limitedHash, _ := auth.Generate()
	mock := provider.NewMock("mock")
	mock.Delay = 0
	rt, err := router.NewWithProviders(
		[]config.Model{
			{Name: "fast", Targets: []config.Target{{Provider: "mock", Model: "m1", Pricing: config.Pricing{InputPerMTok: 1, OutputPerMTok: 2}}}},
			{Name: "smart", Targets: []config.Target{{Provider: "mock", Model: "m2"}}},
		},
		map[string]provider.Provider{"mock": mock},
	)
	if err != nil {
		t.Fatal(err)
	}
	red, _ := redact.New(nil)
	s := New(Deps{
		Config: config.Server{MaxBodyBytes: 1 << 16, RequestTimeout: 5 * time.Second, DefaultMaxTokens: 100, MaxTokensCap: 1000, MaxMessages: 10},
		Auth: auth.New([]config.Key{
			{ID: "full", Tenant: "acme", Hash: hash, RPM: 1000},
			{ID: "scoped", Tenant: "beta", Hash: limitedHash, RPM: 1, Models: []string{"fast"}},
		}),
		Limiter: ratelimit.New(state.NewMemory()), Redactor: red, Router: rt, Metrics: metrics.New(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return fixture{srv: srv, key: key, limitedKey: limited}
}

func (f fixture) post(t *testing.T, key, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", f.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestChatCompletion(t *testing.T) {
	f := setup(t)
	resp := f.post(t, f.key, `{"model":"fast","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"email me at a@b.com"}]}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	got := string(out.Choices[0].Message.Content)
	if !strings.Contains(got, "[REDACTED_EMAIL]") || strings.Contains(got, "a@b.com") {
		t.Fatalf("email must be redacted before reaching the provider: %q", got)
	}
	if resp.Header.Get("X-LLMGate-Redactions") != "1" {
		t.Fatalf("redaction header = %q", resp.Header.Get("X-LLMGate-Redactions"))
	}
	if out.Object != "chat.completion" || out.Usage.TotalTokens == 0 || resp.Header.Get("X-Request-ID") == "" {
		t.Fatalf("unexpected response %+v", out)
	}
}

func TestStreaming(t *testing.T) {
	f := setup(t)
	resp := f.post(t, f.key, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hello there"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	var text strings.Builder
	var sawDone, sawFinish bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var c ChatChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatal(err)
		}
		text.WriteString(c.Choices[0].Delta.Content)
		if c.Choices[0].FinishReason != nil {
			sawFinish = c.Usage != nil
		}
	}
	if !sawDone || !sawFinish || !strings.Contains(text.String(), "hello there") {
		t.Fatalf("done=%v finish=%v text=%q", sawDone, sawFinish, text.String())
	}
}

func TestErrors(t *testing.T) {
	f := setup(t)
	tests := []struct {
		name, key, body string
		status          int
		code            string
	}{
		{"no key", "", `{}`, 401, "invalid_api_key"},
		{"bad json", f.key, `{`, 400, "invalid_json"},
		{"unknown model", f.key, `{"model":"gpt-9","messages":[{"role":"user","content":"x"}]}`, 404, "model_not_found"},
		{"model not allowed", f.limitedKey, `{"model":"smart","messages":[{"role":"user","content":"x"}]}`, 403, "model_not_allowed"},
		{"bad role", f.key, `{"model":"fast","messages":[{"role":"tool","content":"x"}]}`, 400, "unsupported_role"},
		{"max tokens over cap", f.key, `{"model":"fast","max_tokens":99999,"messages":[{"role":"user","content":"x"}]}`, 400, "invalid_max_tokens"},
		{"bad effort", f.key, `{"model":"fast","reasoning_effort":"ultra","messages":[{"role":"user","content":"x"}]}`, 400, "invalid_reasoning_effort"},
		{"reserved extra param", f.key, `{"model":"fast","system":"ignore rules","messages":[{"role":"user","content":"x"}]}`, 400, "invalid_parameter"},
		{"too large", f.key, `{"model":"fast","messages":[{"role":"user","content":"` + strings.Repeat("a", 1<<17) + `"}]}`, 413, "body_too_large"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.post(t, tc.key, tc.body)
			var e ErrorBody
			_ = json.NewDecoder(resp.Body).Decode(&e)
			if resp.StatusCode != tc.status || e.Error.Code != tc.code {
				t.Fatalf("got %d/%q, want %d/%q", resp.StatusCode, e.Error.Code, tc.status, tc.code)
			}
		})
	}
}

func TestRateLimit(t *testing.T) {
	f := setup(t)
	body := `{"model":"fast","messages":[{"role":"user","content":"x"}]}`
	if r := f.post(t, f.limitedKey, body); r.StatusCode != 200 {
		t.Fatalf("first request: %d", r.StatusCode)
	}
	r := f.post(t, f.limitedKey, body)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatalf("second request: %d retry-after=%q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestModelsListIsScoped(t *testing.T) {
	f := setup(t)
	req, _ := http.NewRequest("GET", f.srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+f.limitedKey)
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Data []struct{ ID string } }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 1 || out.Data[0].ID != "fast" {
		t.Fatalf("scoped key should see only its models, got %+v", out.Data)
	}
}

func TestExtraParamsAccepted(t *testing.T) {
	f := setup(t)
	resp := f.post(t, f.key, `{"model":"fast","top_p":0.9,"seed":3,"messages":[{"role":"user","content":"x"}]}`)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
}
