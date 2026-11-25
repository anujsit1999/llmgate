// Package ratelimit enforces per-key requests-per-minute limits with a
// sliding window. Counters live in a state.Store, so the same limit holds
// across every replica when the Redis backend is used.
package ratelimit

import (
	"context"
	"time"

	"github.com/anujsit1999/llmgate/state"
)

// Limiter enforces requests-per-minute per key.
type Limiter struct {
	store state.Store
}

// New returns a limiter backed by store.
func New(store state.Store) *Limiter { return &Limiter{store: store} }

// Allow reports whether key may make a request now. If not, it returns how
// long the caller should wait. A store error is returned with ok=true so the
// caller can decide to fail open (llmgate does, and logs it): an unavailable
// Redis should degrade limits, not take the API down.
func (l *Limiter) Allow(ctx context.Context, key string, rpm int) (bool, time.Duration, error) {
	if rpm <= 0 {
		return true, 0, nil
	}
	ok, wait, err := l.store.Take(ctx, "rl:"+key, float64(rpm))
	if err != nil {
		return true, 0, err
	}
	return ok, wait, nil
}
