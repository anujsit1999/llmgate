package router

import (
	"context"
	"errors"
	"testing"

	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/provider"
)

func newRouter(t *testing.T, primary, backup *provider.Mock) *Router {
	t.Helper()
	r, err := NewWithProviders(
		[]config.Model{{Name: "smart", Targets: []config.Target{
			{Provider: "primary", Model: "big"},
			{Provider: "backup", Model: "small"},
		}}},
		map[string]provider.Provider{"primary": primary, "backup": backup},
	)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var req = provider.Request{MaxTokens: 10, Messages: []provider.Message{{Role: "user", Content: "hi"}}}

func TestFallbackOnRetryableError(t *testing.T) {
	primary, backup := provider.NewMock("primary"), provider.NewMock("backup")
	primary.Fail = &provider.Error{Provider: "primary", Status: 503, Retryable: true, Err: errors.New("down")}
	r := newRouter(t, primary, backup)

	resp, target, attempts, err := r.Complete(context.Background(), "smart", req)
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider.Name() != "backup" || len(attempts) != 2 {
		t.Fatalf("target=%s attempts=%d", target.Provider.Name(), len(attempts))
	}
	if resp.Content != "mock(small) received: hi" {
		t.Fatalf("content = %q", resp.Content)
	}
}

func TestNoFallbackOnClientError(t *testing.T) {
	primary, backup := provider.NewMock("primary"), provider.NewMock("backup")
	primary.Fail = &provider.Error{Provider: "primary", Status: 400, Retryable: false, Err: errors.New("bad")}
	r := newRouter(t, primary, backup)

	if _, _, attempts, err := r.Complete(context.Background(), "smart", req); err == nil || len(attempts) != 1 {
		t.Fatalf("err=%v attempts=%d; a 400 must not fall back", err, len(attempts))
	}
}

func TestStreamFallbackBeforeFirstChunk(t *testing.T) {
	primary, backup := provider.NewMock("primary"), provider.NewMock("backup")
	backup.Delay = 0
	primary.Fail = &provider.Error{Provider: "primary", Status: 529, Retryable: true, Err: errors.New("overloaded")}
	r := newRouter(t, primary, backup)

	var got string
	target, _, err := r.Stream(context.Background(), "smart", req, func(c provider.Chunk) error {
		got += c.Delta
		return nil
	})
	if err != nil || target.Provider.Name() != "backup" || got == "" {
		t.Fatalf("err=%v target=%v got=%q", err, target.Provider, got)
	}
}

func TestUnknownModel(t *testing.T) {
	r := newRouter(t, provider.NewMock("primary"), provider.NewMock("backup"))
	if _, _, _, err := r.Complete(context.Background(), "nope", req); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v", err)
	}
}

func TestParamsMerging(t *testing.T) {
	tgt := Target{Target: config.Target{
		Model:        "llama",
		Params:       map[string]any{"top_p": 0.9, "seed": 7},
		ClientParams: []string{"seed", "top_k"},
	}}
	in := req
	in.Extra = map[string]any{"seed": 1, "top_k": 40, "frequency_penalty": 2}
	out := withTarget(in, tgt)

	if out.Extra["top_p"] != 0.9 || out.Extra["seed"] != 7 {
		t.Fatalf("config params must apply and win over client: %v", out.Extra)
	}
	if out.Extra["top_k"] != 40 {
		t.Fatalf("allowlisted client param dropped: %v", out.Extra)
	}
	if _, ok := out.Extra["frequency_penalty"]; ok {
		t.Fatalf("non-allowlisted client param forwarded: %v", out.Extra)
	}
	if len(in.Extra) != 3 {
		t.Fatal("withTarget must not mutate the caller's map")
	}
}
