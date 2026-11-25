package ratelimit

import (
	"context"
	"testing"

	"github.com/anujsit1999/llmgate/state"
)

func TestAllow(t *testing.T) {
	ctx := context.Background()
	l := New(state.NewMemory())
	for i := range 3 {
		if ok, _, _ := l.Allow(ctx, "k", 3); !ok {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	ok, wait, err := l.Allow(ctx, "k", 3)
	if err != nil || ok || wait <= 0 {
		t.Fatalf("4th request should be limited: ok=%v wait=%v err=%v", ok, wait, err)
	}
	if ok, _, _ := l.Allow(ctx, "other", 3); !ok {
		t.Fatal("keys must have independent windows")
	}
	if ok, _, _ := l.Allow(ctx, "unlimited", 0); !ok {
		t.Fatal("rpm 0 means unlimited")
	}
}
