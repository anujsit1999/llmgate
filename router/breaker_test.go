package router

import (
	"testing"
	"time"

	"github.com/anujsit1999/llmgate/config"
)

// fakeClock lets tests move time forward instantly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker() (*breaker, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	b := newBreaker(config.CircuitBreaker{FailureThreshold: 3, Window: 5 * time.Minute, Cooldown: 30 * time.Second, MaxCooldown: 2 * time.Minute})
	b.now = clk.now
	return b, clk
}

func TestBreakerTripsAfterThresholdWithinWindow(t *testing.T) {
	b, clk := newTestBreaker()
	b.Failure()
	b.Failure()
	if !b.Allow() {
		t.Fatal("2 failures < threshold 3: should stay closed")
	}
	b.Failure()
	if b.Available() || b.Allow() {
		t.Fatal("3 failures within 5m should open the circuit")
	}
	clk.advance(29 * time.Second)
	if b.Available() {
		t.Fatal("still cooling down")
	}
	clk.advance(2 * time.Second)
	if !b.Allow() {
		t.Fatal("after cooldown one probe must be admitted")
	}
	if b.Allow() {
		t.Fatal("only one probe at a time in half-open")
	}
	b.Success()
	if b.snapshot().State != Closed || !b.Allow() {
		t.Fatal("successful probe should close the circuit")
	}
}

func TestBreakerOldFailuresExpire(t *testing.T) {
	b, clk := newTestBreaker()
	b.Failure()
	b.Failure()
	clk.advance(6 * time.Minute) // outside the 5m window
	b.Failure()
	if b.snapshot().State != Closed {
		t.Fatal("failures older than the window must not count")
	}
}

func TestBreakerFailedProbeBacksOff(t *testing.T) {
	b, clk := newTestBreaker()
	for range 3 {
		b.Failure()
	}
	clk.advance(31 * time.Second)
	if !b.Allow() {
		t.Fatal("probe expected")
	}
	b.Failure() // probe fails -> reopen with doubled cooldown (60s)
	clk.advance(45 * time.Second)
	if b.Available() {
		t.Fatal("cooldown should have doubled to 60s")
	}
	clk.advance(20 * time.Second)
	if !b.Available() {
		t.Fatal("should be probe-able after doubled cooldown")
	}
}

func TestBreakerReleaseFreesProbe(t *testing.T) {
	b, clk := newTestBreaker()
	for range 3 {
		b.Failure()
	}
	clk.advance(31 * time.Second)
	b.Allow()
	b.Release() // client cancelled mid-probe
	if !b.Allow() {
		t.Fatal("a released probe must let the next request probe")
	}
}

func TestBreakerDisabled(t *testing.T) {
	b, _ := newTestBreaker()
	b.disabled = true
	for range 10 {
		b.Failure()
	}
	if !b.Allow() || !b.Available() {
		t.Fatal("disabled breaker must always allow")
	}
}
