package config

import (
	"os"
	"strings"
	"testing"
)

func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile("../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("config.example.yaml must stay valid: %v", err)
	}
	if c.Server.Addr == "" || len(c.Models) == 0 {
		t.Fatal("defaults not applied")
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct{ name, yaml, wantErr string }{
		{"unknown field", "nope: 1", "field nope not found"},
		{"unknown provider ref", `
models:
  - name: m
    targets: [{provider: ghost, model: x}]`, `unknown provider "ghost"`},
		{"bad hash", `
providers: {p: {type: mock}}
models: [{name: m, targets: [{provider: p, model: x}]}]
keys: [{id: k, tenant: t, hash: plain}]`, "hash must be"},
		{"fallback on non-anthropic", `
providers: {p: {type: openai}}
models: [{name: m, targets: [{provider: p, model: x, server_side_fallback: true}]}]`, "server_side_fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestPricingCost(t *testing.T) {
	p := Pricing{InputPerMTok: 4, OutputPerMTok: 20}
	if got := p.Cost(1_000_000, 500_000); got != 14 {
		t.Fatalf("cost = %v, want 14", got)
	}
}

func TestClientParamsSemantics(t *testing.T) {
	c, err := Parse([]byte(`
providers: {p: {type: mock}}
models:
  - name: any
    targets: [{provider: p, model: x}]
  - name: none
    targets: [{provider: p, model: x, client_params: []}]
  - name: some
    targets: [{provider: p, model: x, client_params: [seed]}]`))
	if err != nil {
		t.Fatal(err)
	}
	anyT, noneT, someT := c.Models[0].Targets[0], c.Models[1].Targets[0], c.Models[2].Targets[0]
	if !anyT.AllowsClientParam("seed") || noneT.AllowsClientParam("seed") ||
		!someT.AllowsClientParam("seed") || someT.AllowsClientParam("top_k") {
		t.Fatal("client_params semantics wrong: unset=all, []=none, list=allowlist")
	}
}

func TestCheckParamKey(t *testing.T) {
	for _, ok := range []string{"top_p", "thinking.display", "options.num_ctx"} {
		if err := CheckParamKey(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"model", "messages.0", "max_tokens", "system", "a b", "x..y", "../etc"} {
		if CheckParamKey(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("LG_REGION", "eu-west-1")
	got, err := ExpandEnv("region: ${LG_REGION}\naddr: ${LG_UNSET_ADDR:-:8080}")
	if err != nil || got != "region: eu-west-1\naddr: :8080" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := ExpandEnv("x: ${LG_DEFINITELY_UNSET}"); err == nil {
		t.Fatal("unset variable without default must be an error")
	}
}

func TestDefaultsAndFeatures(t *testing.T) {
	c, err := Parse([]byte(`
providers: {p: {type: mock}}
models: [{name: m, targets: [{provider: p, model: x}], queue: {}}]
features: {rate_limit: false}`))
	if err != nil {
		t.Fatal(err)
	}
	m := c.Models[0]
	if m.Strategy != StrategyPriority || m.CircuitBreaker.FailureThreshold != 5 || m.Queue.MaxDepth != 100 || c.State.Backend != "memory" {
		t.Fatalf("defaults not applied: %+v", m)
	}
	if c.Features.RateLimitOn() || !c.Features.AuthOn() {
		t.Fatal("feature toggles wrong: rate_limit off, everything else on by default")
	}
}

func TestRedisNeedsAddr(t *testing.T) {
	if _, err := Parse([]byte("state: {backend: redis}")); err == nil || !strings.Contains(err.Error(), "state.redis.addr") {
		t.Fatalf("err = %v", err)
	}
}
