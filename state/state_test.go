package state

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/anujsit1999/llmgate/config"
)

// Every backend must pass the same behavioural suite.
func testStore(t *testing.T, s Store, setNow func(time.Time)) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	setNow(t0)

	// Add / Rates
	if err := s.Add(ctx, "dep", map[string]float64{"req": 1, "tok": 100}); err != nil {
		t.Fatal(err)
	}
	_ = s.Add(ctx, "dep", map[string]float64{"req": 1, "tok": 50})
	v, err := s.Rates(ctx, "dep", "req", "tok")
	if err != nil || v[0] != 2 || v[1] != 150 {
		t.Fatalf("rates = %v err=%v, want [2 150]", v, err)
	}

	// Sliding: 30s into the next window, the previous window counts half.
	setNow(t0.Add(90 * time.Second))
	v, _ = s.Rates(ctx, "dep", "req", "tok")
	if v[0] != 1 || v[1] != 75 {
		t.Fatalf("sliding rates = %v, want [1 75]", v)
	}
	// Two windows later everything has aged out.
	setNow(t0.Add(3 * time.Minute))
	if v, _ = s.Rates(ctx, "dep", "req"); v[0] != 0 {
		t.Fatalf("expired rates = %v, want 0", v)
	}

	// Take
	setNow(t0.Add(10 * time.Minute))
	for i := range 3 {
		if ok, _, err := s.Take(ctx, "rl", 3); !ok || err != nil {
			t.Fatalf("take %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, wait, err := s.Take(ctx, "rl", 3)
	if ok || err != nil || wait <= 0 || wait > time.Minute {
		t.Fatalf("4th take: ok=%v wait=%v err=%v", ok, wait, err)
	}
	if ok, _, _ := s.Take(ctx, "other", 3); !ok {
		t.Fatal("keys are independent")
	}
}

func TestMemoryStore(t *testing.T) {
	s := NewMemory()
	testStore(t, s, func(now time.Time) { s.now = func() time.Time { return now } })
}

func TestRedisStore(t *testing.T) {
	mr := miniredis.RunT(t)
	s, err := NewRedis(context.Background(), config.Redis{Addr: mr.Addr(), KeyPrefix: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	testStore(t, s, func(now time.Time) {
		s.now = func() time.Time { return now }
		mr.SetTime(now)
	})
	// Keys carry a Redis Cluster hash tag and expire on their own.
	keys := mr.Keys()
	if len(keys) == 0 || mr.TTL(keys[0]) <= 0 {
		t.Fatalf("keys should exist with a TTL: %v", keys)
	}
}

func TestSharedAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	a, _ := NewRedis(context.Background(), config.Redis{Addr: mr.Addr()})
	b, _ := NewRedis(context.Background(), config.Redis{Addr: mr.Addr()})
	defer a.Close()
	defer b.Close()
	ctx := context.Background()
	if ok, _, _ := a.Take(ctx, "k", 1); !ok {
		t.Fatal("replica A should take the only slot")
	}
	if ok, _, _ := b.Take(ctx, "k", 1); ok {
		t.Fatal("replica B must see A's usage: limit is cluster-wide")
	}
}

func TestNewUnknownBackend(t *testing.T) {
	if _, err := New(context.Background(), config.State{Backend: "etcd"}); err == nil {
		t.Fatal("expected error")
	}
}
