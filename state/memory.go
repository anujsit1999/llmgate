package state

import (
	"context"
	"sync"
	"time"

	"github.com/anujsit1999/llmgate/config"
)

func init() {
	Register("memory", func(context.Context, config.State) (Store, error) { return NewMemory(), nil })
}

// Memory is an in-process Store. Each replica counts independently, so use
// it for single-instance deployments and tests.
type Memory struct {
	mu   sync.Mutex
	m    map[string]*window
	now  func() time.Time
	last time.Time // last sweep of idle keys
}

type window struct {
	start     time.Time
	cur, prev map[string]float64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{m: map[string]*window{}, now: time.Now} }

// get returns key's window rotated to now. Caller holds mu.
func (s *Memory) get(key string, now time.Time) *window {
	start := windowStart(now)
	w, ok := s.m[key]
	if !ok {
		w = &window{start: start, cur: map[string]float64{}, prev: map[string]float64{}}
		s.m[key] = w
	}
	switch {
	case start.Equal(w.start):
	case start.Sub(w.start) == Window:
		w.prev, w.cur, w.start = w.cur, map[string]float64{}, start
	default: // idle for 2+ windows
		w.prev, w.cur, w.start = map[string]float64{}, map[string]float64{}, start
	}
	s.sweep(now)
	return w
}

// sweep drops keys idle for over two windows, bounding memory. Caller holds mu.
func (s *Memory) sweep(now time.Time) {
	if now.Sub(s.last) < Window {
		return
	}
	s.last = now
	for k, w := range s.m {
		if now.Sub(w.start) > 2*Window {
			delete(s.m, k)
		}
	}
}

// Add implements Store.
func (s *Memory) Add(_ context.Context, key string, deltas map[string]float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.get(key, s.now())
	for f, d := range deltas {
		w.cur[f] += d
	}
	return nil
}

// Rates implements Store.
func (s *Memory) Rates(_ context.Context, key string, fields ...string) ([]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	w := s.get(key, now)
	pw := prevWeight(now)
	out := make([]float64, len(fields))
	for i, f := range fields {
		out[i] = w.prev[f]*pw + w.cur[f]
	}
	return out, nil
}

// Take implements Store.
func (s *Memory) Take(_ context.Context, key string, limit float64) (bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	w := s.get(key, now)
	prev, cur := w.prev["n"], w.cur["n"]
	if prev*prevWeight(now)+cur+1 > limit {
		return false, retryAfter(now, prev, cur, limit), nil
	}
	w.cur["n"]++
	return true, 0, nil
}

// Close implements Store.
func (s *Memory) Close() error { return nil }
