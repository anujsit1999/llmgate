// Package state provides the counters that must be shared when llmgate runs
// as several replicas: per-deployment request/token rates and per-key client
// rate limits.
//
// All counters are one-minute sliding windows implemented as a "sliding
// window counter": the current fixed window plus the previous one weighted by
// how much of it still overlaps the last 60 seconds. It needs two numbers per
// key, is accurate to within a few percent, and maps directly onto Redis.
//
// Backends register themselves (like providers), so adding e.g. Memcached or
// DynamoDB is one file:
//
//	memory — in-process; correct for a single replica (default)
//	redis  — shared across replicas; works with Redis, Valkey, ElastiCache, Redis Cluster
package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anujsit1999/llmgate/config"
)

// Window is the length of every sliding window.
const Window = time.Minute

// Store is a sliding-window counter store.
type Store interface {
	// Add increments fields of key in the current window.
	Add(ctx context.Context, key string, deltas map[string]float64) error
	// Rates returns each field's estimated total over the last Window.
	Rates(ctx context.Context, key string, fields ...string) ([]float64, error)
	// Take atomically admits one event for key if the sliding count is below
	// limit. When refused it returns how long until a slot is likely free.
	Take(ctx context.Context, key string, limit float64) (ok bool, retryAfter time.Duration, err error)
	Close() error
}

// Factory builds a Store from config.
type Factory func(ctx context.Context, cfg config.State) (Store, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a backend available to `state.backend` in config.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("state: Register called twice for backend " + name)
	}
	factories[name] = f
}

// New builds the configured backend.
func New(ctx context.Context, cfg config.State) (Store, error) {
	mu.RLock()
	f, ok := factories[cfg.Backend]
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	mu.RUnlock()
	if !ok {
		sort.Strings(names)
		return nil, fmt.Errorf("state: unknown backend %q (available: %s)", cfg.Backend, strings.Join(names, ", "))
	}
	return f(ctx, cfg)
}

// windowStart returns the start of the fixed window containing t.
func windowStart(t time.Time) time.Time { return t.Truncate(Window) }

// prevWeight is how much of the previous window still overlaps the last minute.
func prevWeight(now time.Time) float64 {
	return 1 - float64(now.Sub(windowStart(now)))/float64(Window)
}

// retryAfter estimates when `prev*w(t) + cur + 1 <= limit` first holds.
func retryAfter(now time.Time, prev, cur, limit float64) time.Duration {
	elapsed := now.Sub(windowStart(now))
	if cur+1 > limit || prev <= 0 {
		// Must wait for the window to roll over (cur becomes prev).
		return max(Window-elapsed, 100*time.Millisecond)
	}
	// Solve prev*(1 - e/Window) + cur + 1 <= limit for e.
	need := time.Duration(float64(Window) * (1 - (limit-1-cur)/prev))
	return max(need-elapsed, 100*time.Millisecond)
}
