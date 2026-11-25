// Package router maps client-facing model aliases to upstream deployments.
//
// For every request it:
//  1. orders the model's targets with the configured strategy
//     (priority, round_robin, weighted, least_busy, usage_based),
//  2. skips targets whose circuit breaker is open or that are over their
//     tpm/rpm capacity,
//  3. appends targets of the model's fallback models,
//  4. tries candidates in order, moving on after retryable failures and
//     feeding every outcome back into the circuit breakers.
package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/provider"
	"github.com/anujsit1999/llmgate/state"
)

var (
	// ErrUnknownModel is returned for an alias that is not configured.
	ErrUnknownModel = errors.New("unknown model")
	// ErrNoHealthyUpstream means every candidate was circuit-open or over capacity.
	ErrNoHealthyUpstream = errors.New("no healthy upstream available")
	// ErrQueueFull means the model's wait queue is at max_depth.
	ErrQueueFull = fmt.Errorf("%w: wait queue full", ErrNoHealthyUpstream)
)

// Option customises a Router.
type Option func(*Router)

// WithStore sets the shared counter store (default: in-memory).
func WithStore(s state.Store) Option { return func(r *Router) { r.store = s } }

// WithStateErrorHandler is called when the store fails; requests proceed
// (fail open). Use it to log or count store outages.
func WithStateErrorHandler(fn func(error)) Option { return func(r *Router) { r.onStateErr = fn } }

// WithoutCircuitBreaker disables circuit breaking: deployments are never
// skipped for health, only tried in strategy order with retry/fallback.
func WithoutCircuitBreaker() Option { return func(r *Router) { r.noBreaker = true } }

// queuePoll is how often a queued request re-checks capacity.
var queuePoll = 250 * time.Millisecond

// deployment is the runtime state of one (provider, upstream model) pair.
// It is shared by every alias that routes to it, so health and load are
// tracked per real deployment, not per alias.
type deployment struct {
	key     string
	breaker *breaker
	usage   *usage
}

// Target is a model's routing entry for one deployment.
type Target struct {
	Provider provider.Provider
	config.Target
	dep *deployment
}

// Attempt records one candidate, for logs and metrics. Skipped is set
// ("circuit_open", "over_capacity") when the target was not called.
type Attempt struct {
	Target   Target
	Err      error
	Duration time.Duration
	Skipped  string
}

// Called reports whether the upstream was actually invoked.
func (a Attempt) Called() bool { return a.Skipped == "" }

type route struct {
	targets    []*Target
	strategy   strategy
	fallbacks  []string
	queue      *config.Queue
	waiting    atomic.Int64
	needsRates bool // usage_based strategy or any tpm/rpm limit
}

// Router holds the routing table and per-deployment state.
type Router struct {
	routes      map[string]*route
	deployments map[string]*deployment
	store       state.Store
	onStateErr  func(error)
	noBreaker   bool
}

// New builds a router from config, constructing provider adapters through
// the provider registry. Only providers referenced by a model are built, so
// an unused provider with missing credentials does not block startup.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*Router, error) {
	used := map[string]bool{}
	for _, m := range cfg.Models {
		for _, t := range m.Targets {
			used[t.Provider] = true
		}
	}
	providers := make(map[string]provider.Provider, len(used))
	for name := range used {
		p, err := provider.Build(ctx, name, cfg.Providers[name])
		if err != nil {
			return nil, err
		}
		providers[name] = p
	}
	return NewWithProviders(cfg.Models, providers, opts...)
}

// NewWithProviders builds a router from pre-constructed providers (useful in tests).
func NewWithProviders(models []config.Model, providers map[string]provider.Provider, opts ...Option) (*Router, error) {
	r := &Router{routes: map[string]*route{}, deployments: map[string]*deployment{}}
	for _, o := range opts {
		o(r)
	}
	if r.store == nil {
		r.store = state.NewMemory()
	}
	for _, m := range models {
		cb := config.DefaultCircuitBreaker
		if m.CircuitBreaker != nil {
			cb = *m.CircuitBreaker
		}
		rt := &route{fallbacks: m.Fallbacks, queue: m.Queue, needsRates: m.Strategy == config.StrategyUsageBased}
		for _, t := range m.Targets {
			p, ok := providers[t.Provider]
			if !ok {
				return nil, errors.New("model " + m.Name + ": unknown provider " + t.Provider)
			}
			key := t.Provider + "/" + t.Model
			dep, ok := r.deployments[key]
			if !ok {
				b := newBreaker(cb)
				b.disabled = r.noBreaker
				dep = &deployment{key: key, breaker: b,
					usage: &usage{key: "dep:" + key, store: r.store, onErr: r.onStateErr}}
				r.deployments[key] = dep
			}
			if t.Weight == 0 {
				t.Weight = 1
			}
			rt.targets = append(rt.targets, &Target{Provider: p, Target: t, dep: dep})
			if t.TPM > 0 || t.RPM > 0 {
				rt.needsRates = true
			}
		}
		rt.strategy = newStrategy(m.Strategy, len(rt.targets))
		r.routes[m.Name] = rt
	}
	return r, nil
}

// Models lists configured aliases, sorted.
func (r *Router) Models() []string {
	out := make([]string, 0, len(r.routes))
	for name := range r.routes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether an alias exists.
func (r *Router) Has(model string) bool { _, ok := r.routes[model]; return ok }

// plan returns candidates in try-order, plus attempts for skipped targets.
func (r *Router) plan(ctx context.Context, model string, estTokens int) ([]*Target, []Attempt) {
	var cands []*Target
	var skipped []Attempt
	seenModel := map[string]bool{}
	seenDep := map[*deployment]bool{}

	var walk func(name string)
	walk = func(name string) {
		rt, ok := r.routes[name]
		if !ok || seenModel[name] {
			return
		}
		seenModel[name] = true
		loads := make(map[*Target]load, len(rt.targets))
		for _, t := range rt.targets {
			l := load{inFlight: float64(t.dep.usage.inFlight.Load())}
			if rt.needsRates {
				l.rpm, l.tpm = t.dep.usage.rates(ctx)
			}
			loads[t] = l
		}
		for _, t := range rt.strategy.order(rt.targets, loads) {
			if seenDep[t.dep] {
				continue
			}
			seenDep[t.dep] = true
			switch {
			case !t.dep.breaker.Available():
				skipped = append(skipped, Attempt{Target: *t, Skipped: "circuit_open"})
			case !hasCapacity(t, loads[t], estTokens):
				skipped = append(skipped, Attempt{Target: *t, Skipped: "over_capacity"})
			default:
				cands = append(cands, t)
			}
		}
		for _, f := range rt.fallbacks {
			walk(f)
		}
	}
	walk(model)
	return cands, skipped
}

// callFunc invokes one target. It returns tokens consumed and whether any
// output reached the client (after which fallback is impossible).
type callFunc func(ctx context.Context, t *Target, req provider.Request) (tokens int, started bool, err error)

func (r *Router) execute(ctx context.Context, model string, req provider.Request, call callFunc) (Target, []Attempt, error) {
	if !r.Has(model) {
		return Target{}, nil, ErrUnknownModel
	}
	est := estimateTokens(req)
	cands, attempts := r.plan(ctx, model, est)
	if len(cands) == 0 && onlyOverCapacity(attempts) {
		var err error
		if cands, attempts, err = r.wait(ctx, model, est); err != nil {
			return Target{}, attempts, err
		}
	}
	var last Target
	lastErr := ErrNoHealthyUpstream

	for _, t := range cands {
		if !t.dep.breaker.Allow() { // lost a half-open probe race
			attempts = append(attempts, Attempt{Target: *t, Skipped: "circuit_open"})
			continue
		}
		start := time.Now()
		t.dep.usage.begin(ctx)
		tokens, started, err := call(ctx, t, withTarget(req, *t))
		t.dep.usage.end(ctx, tokens)
		attempts = append(attempts, Attempt{Target: *t, Err: err, Duration: time.Since(start)})
		last, lastErr = *t, err

		switch {
		case err == nil:
			t.dep.breaker.Success()
			return *t, attempts, nil
		case errors.Is(ctx.Err(), context.Canceled):
			t.dep.breaker.Release() // client left: says nothing about upstream health
			return *t, attempts, err
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			t.dep.breaker.Failure() // upstream too slow; no time left to fall back
			return *t, attempts, err
		case provider.IsRetryable(err):
			t.dep.breaker.Failure()
			if started {
				return *t, attempts, err
			}
			// otherwise continue with the next candidate
		default:
			// The upstream answered and rejected the request itself (e.g. 400):
			// the deployment is healthy and another one would reject it too.
			t.dep.breaker.Success()
			return *t, attempts, err
		}
	}
	return last, attempts, lastErr
}

// onlyOverCapacity reports whether candidates were skipped and all for
// capacity (not outages) — the only case worth queueing for.
func onlyOverCapacity(attempts []Attempt) bool {
	for _, a := range attempts {
		if a.Skipped != "over_capacity" {
			return false
		}
	}
	return len(attempts) > 0
}

// wait holds a request in the model's bounded queue until capacity frees up,
// max_wait elapses, or the client goes away.
func (r *Router) wait(ctx context.Context, model string, est int) ([]*Target, []Attempt, error) {
	rt := r.routes[model]
	q := rt.queue
	if q == nil {
		return nil, nil, ErrNoHealthyUpstream
	}
	if rt.waiting.Add(1) > int64(q.MaxDepth) {
		rt.waiting.Add(-1)
		return nil, nil, ErrQueueFull
	}
	defer rt.waiting.Add(-1)

	deadline := time.NewTimer(q.MaxWait)
	defer deadline.Stop()
	tick := time.NewTicker(queuePoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-deadline.C:
			return nil, nil, ErrNoHealthyUpstream
		case <-tick.C:
			cands, attempts := r.plan(ctx, model, est)
			if len(cands) > 0 || !onlyOverCapacity(attempts) {
				return cands, attempts, nil
			}
		}
	}
}

// QueueDepth reports how many requests are waiting for a model on this replica.
func (r *Router) QueueDepth(model string) int64 {
	if rt, ok := r.routes[model]; ok {
		return rt.waiting.Load()
	}
	return 0
}

// Complete routes a non-streaming request.
func (r *Router) Complete(ctx context.Context, model string, req provider.Request) (*provider.Response, Target, []Attempt, error) {
	var resp *provider.Response
	target, attempts, err := r.execute(ctx, model, req, func(ctx context.Context, t *Target, req provider.Request) (int, bool, error) {
		res, err := t.Provider.Complete(ctx, req)
		if err != nil {
			return 0, false, err
		}
		resp = res
		return res.Usage.InputTokens + res.Usage.OutputTokens, false, nil
	})
	if err != nil {
		return nil, target, attempts, err
	}
	return resp, target, attempts, nil
}

// Stream routes a streaming request. Fallback is possible only until the
// first chunk has been sent to the client.
func (r *Router) Stream(ctx context.Context, model string, req provider.Request, emit func(provider.Chunk) error) (Target, []Attempt, error) {
	return r.execute(ctx, model, req, func(ctx context.Context, t *Target, req provider.Request) (int, bool, error) {
		started, tokens := false, 0
		err := t.Provider.Stream(ctx, req, func(c provider.Chunk) error {
			started = true
			if c.Usage != nil {
				tokens = c.Usage.InputTokens + c.Usage.OutputTokens
			}
			return emit(c)
		})
		return tokens, started, err
	})
}

// estimateTokens approximates prompt size (~4 characters per token) for
// capacity checks before the real count is known.
func estimateTokens(req provider.Request) int {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content)
	}
	return n/4 + 1
}

// withTarget specialises a request for one target: upstream model id,
// client extras filtered by the target's allowlist, then the target's own
// params layered on top (config wins over the client).
func withTarget(req provider.Request, t Target) provider.Request {
	req.Model = t.Model
	req.ServerSideFallback = t.ServerSideFallback
	extra := make(map[string]any, len(req.Extra)+len(t.Params))
	for k, v := range req.Extra {
		if t.AllowsClientParam(k) {
			extra[k] = v
		}
	}
	for k, v := range t.Params {
		extra[k] = v
	}
	req.Extra = extra
	return req
}

// UpstreamStatus is a point-in-time view of one deployment.
type UpstreamStatus struct {
	Deployment     string     `json:"deployment"`
	State          string     `json:"circuit_state"`
	RecentFailures int        `json:"recent_failures"`
	OpenUntil      *time.Time `json:"open_until,omitempty"`
	InFlight       int64      `json:"in_flight"`
	RequestsPerMin float64    `json:"requests_per_min"`
	TokensPerMin   float64    `json:"tokens_per_min"`
}

// Upstreams reports the health and load of every deployment.
func (r *Router) Upstreams(ctx context.Context) []UpstreamStatus {
	out := make([]UpstreamStatus, 0, len(r.deployments))
	for _, d := range r.deployments {
		b := d.breaker.snapshot()
		rpm, tpm := d.usage.rates(ctx)
		s := UpstreamStatus{
			Deployment: d.key, State: b.State.String(), RecentFailures: b.Failures,
			InFlight: d.usage.inFlight.Load(), RequestsPerMin: rpm, TokensPerMin: tpm,
		}
		if !b.OpenUntil.IsZero() {
			s.OpenUntil = &b.OpenUntil
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deployment < out[j].Deployment })
	return out
}
