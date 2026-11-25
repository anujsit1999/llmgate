package router

import (
	"sync"
	"time"

	"github.com/anujsit1999/llmgate/config"
)

// BreakerState is the circuit breaker's state.
type BreakerState int

// Circuit breaker states.
const (
	Closed   BreakerState = iota // healthy: traffic flows
	Open                         // tripped: traffic is skipped until the cooldown ends
	HalfOpen                     // cooldown over: one probe request decides
)

func (s BreakerState) String() string {
	return [...]string{"closed", "open", "half_open"}[s]
}

// breaker is a per-deployment circuit breaker.
//
//	closed --(N failures within window)--> open --(cooldown)--> half_open
//	half_open --(probe succeeds)--> closed
//	half_open --(probe fails)-----> open, with the cooldown doubled (up to max)
//
// It is safe for concurrent use.
type breaker struct {
	mu        sync.Mutex
	cfg       config.CircuitBreaker
	state     BreakerState
	failures  []time.Time // failure timestamps inside the window
	openUntil time.Time
	cooldown  time.Duration // current cooldown; grows on repeated trips
	probing   bool          // a half-open probe is in flight
	disabled  bool          // circuit breaking switched off: always closed
	now       func() time.Time
}

func newBreaker(cfg config.CircuitBreaker) *breaker {
	return &breaker{cfg: cfg, cooldown: cfg.Cooldown, now: time.Now}
}

// Available reports, without side effects, whether a request could be sent
// now. Used to rank candidates before committing to one.
func (b *breaker) Available() bool {
	if b.disabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		return !b.now().Before(b.openUntil)
	case HalfOpen:
		return !b.probing
	default:
		return true
	}
}

// Allow is called right before sending. In half-open state it admits exactly
// one probe; concurrent callers are turned away until the probe resolves.
func (b *breaker) Allow() bool {
	if b.disabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Open:
		if b.now().Before(b.openUntil) {
			return false
		}
		b.state = HalfOpen
		b.probing = false
		fallthrough
	case HalfOpen:
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return true
	}
}

// Success records a healthy response and closes the circuit.
func (b *breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures, b.probing, b.cooldown = Closed, nil, false, b.cfg.Cooldown
}

// Failure records an upstream failure and may trip the circuit.
func (b *breaker) Failure() {
	if b.disabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.state == HalfOpen {
		// The probe failed: back off harder before trying again.
		b.cooldown = min(b.cooldown*2, b.cfg.MaxCooldown)
		b.trip(now)
		return
	}
	cutoff := now.Add(-b.cfg.Window)
	kept := b.failures[:0]
	for _, t := range b.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.failures = append(kept, now)
	if len(b.failures) >= b.cfg.FailureThreshold {
		b.trip(now)
	}
}

// Release ends a half-open probe that produced no verdict (e.g. the client
// cancelled), so the next request can probe instead.
func (b *breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (b *breaker) trip(now time.Time) {
	b.state, b.openUntil, b.failures, b.probing = Open, now.Add(b.cooldown), nil, false
}

type breakerSnapshot struct {
	State     BreakerState
	Failures  int
	OpenUntil time.Time
}

func (b *breaker) snapshot() breakerSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := breakerSnapshot{State: b.state, Failures: len(b.failures)}
	if b.state == Open {
		s.OpenUntil = b.openUntil
	}
	return s
}
