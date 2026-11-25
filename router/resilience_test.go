package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/provider"
)

var down = &provider.Error{Provider: "x", Status: 503, Retryable: true, Err: errors.New("down")}

func TestCircuitOpensThenModelFallback(t *testing.T) {
	primary, backup := provider.NewMock("primary"), provider.NewMock("backup")
	primary.Fail = down
	cb := &config.CircuitBreaker{FailureThreshold: 2, Window: 5 * time.Minute, Cooldown: time.Minute, MaxCooldown: time.Minute}
	r, err := NewWithProviders([]config.Model{
		{Name: "smart", Targets: []config.Target{{Provider: "primary", Model: "big"}}, Fallbacks: []string{"cheap"}, CircuitBreaker: cb},
		{Name: "cheap", Targets: []config.Target{{Provider: "backup", Model: "small"}}},
	}, map[string]provider.Provider{"primary": primary, "backup": backup})
	if err != nil {
		t.Fatal(err)
	}

	// Two failing calls: both fall back to the "cheap" model and trip the breaker.
	for i := range 2 {
		_, target, attempts, err := r.Complete(context.Background(), "smart", req)
		if err != nil || target.Provider.Name() != "backup" || !attempts[0].Called() {
			t.Fatalf("call %d: err=%v target=%v", i, err, target.Provider)
		}
	}
	// Third call: primary is skipped without being called (circuit open).
	_, target, attempts, err := r.Complete(context.Background(), "smart", req)
	if err != nil || target.Provider.Name() != "backup" {
		t.Fatalf("err=%v", err)
	}
	if attempts[0].Skipped != "circuit_open" {
		t.Fatalf("primary should be skipped as circuit_open, got %+v", attempts[0])
	}
	st := r.Upstreams(context.Background())
	if st[0].Deployment != "backup/small" || st[1].State != "open" {
		t.Fatalf("upstream status wrong: %+v", st)
	}
}

func TestAllCircuitsOpenFailsFast(t *testing.T) {
	m := provider.NewMock("only")
	m.Fail = down
	cb := &config.CircuitBreaker{FailureThreshold: 1, Window: time.Minute, Cooldown: time.Minute, MaxCooldown: time.Minute}
	r, _ := NewWithProviders([]config.Model{{Name: "m", Targets: []config.Target{{Provider: "only", Model: "x"}}, CircuitBreaker: cb,
		Queue: &config.Queue{MaxWait: time.Second, MaxDepth: 10}}}, map[string]provider.Provider{"only": m})

	_, _, _, _ = r.Complete(context.Background(), "m", req) // trips
	start := time.Now()
	_, _, _, err := r.Complete(context.Background(), "m", req)
	if !errors.Is(err, ErrNoHealthyUpstream) {
		t.Fatalf("err = %v, want ErrNoHealthyUpstream", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("an outage must fail fast, not wait in the queue")
	}
}

func TestQueueWaitsForCapacity(t *testing.T) {
	queuePoll = 10 * time.Millisecond
	m := provider.NewMock("p")
	m.Delay = 0
	r, _ := NewWithProviders([]config.Model{{
		Name:    "m",
		Targets: []config.Target{{Provider: "p", Model: "x", RPM: 1}},
		Queue:   &config.Queue{MaxWait: 200 * time.Millisecond, MaxDepth: 1},
	}}, map[string]provider.Provider{"p": m})

	if _, _, _, err := r.Complete(context.Background(), "m", req); err != nil {
		t.Fatal(err)
	}
	// rpm 1 is used up; this request waits max_wait and then gives up.
	start := time.Now()
	_, _, _, err := r.Complete(context.Background(), "m", req)
	if !errors.Is(err, ErrNoHealthyUpstream) || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("err=%v waited=%v: should queue ~max_wait then fail", err, time.Since(start))
	}
}

func TestQueueFull(t *testing.T) {
	m := provider.NewMock("p")
	r, _ := NewWithProviders([]config.Model{{
		Name:    "m",
		Targets: []config.Target{{Provider: "p", Model: "x", RPM: 1}},
		Queue:   &config.Queue{MaxWait: time.Second, MaxDepth: 1},
	}}, map[string]provider.Provider{"p": m})
	m.Delay = 0
	_, _, _, _ = r.Complete(context.Background(), "m", req)

	r.routes["m"].waiting.Store(1) // simulate one request already waiting
	if _, _, _, err := r.Complete(context.Background(), "m", req); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", err)
	}
}

func TestClientErrorDoesNotTripBreaker(t *testing.T) {
	m := provider.NewMock("p")
	m.Fail = &provider.Error{Provider: "p", Status: 400, Err: errors.New("bad request")}
	cb := &config.CircuitBreaker{FailureThreshold: 1, Window: time.Minute, Cooldown: time.Minute, MaxCooldown: time.Minute}
	r, _ := NewWithProviders([]config.Model{{Name: "m", Targets: []config.Target{{Provider: "p", Model: "x"}}, CircuitBreaker: cb}},
		map[string]provider.Provider{"p": m})
	for range 3 {
		_, _, _, _ = r.Complete(context.Background(), "m", req)
	}
	if st := r.Upstreams(context.Background())[0].State; st != "closed" {
		t.Fatalf("400s are the caller's fault; circuit should stay closed, got %s", st)
	}
}
