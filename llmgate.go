// Package llmgate is an embeddable LLM gateway: one OpenAI-compatible API in
// front of many providers, with routing strategies, circuit breaking,
// fallbacks, capacity-aware queueing, rate limits, redaction and metrics.
//
// Run it as a binary (cmd/llmgate) or embed it in your own Go service:
//
//	cfg, _ := config.Load("config.yaml")
//	gw, err := llmgate.New(ctx, cfg,
//		llmgate.WithLogger(myLogger),
//		llmgate.WithProvider("inhouse", myProvider), // any provider.Provider
//	)
//	if err != nil { ... }
//	defer gw.Close()
//	mux.Handle("/llm/", http.StripPrefix("/llm", gw.Handler()))
//
// Every subsystem can be switched off through cfg.Features, and the lower
// level packages (provider, router, state, redact, auth, ratelimit, config)
// are usable on their own.
package llmgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/anujsit1999/llmgate/auth"
	"github.com/anujsit1999/llmgate/config"
	"github.com/anujsit1999/llmgate/internal/metrics"
	"github.com/anujsit1999/llmgate/internal/server"
	"github.com/anujsit1999/llmgate/provider"
	"github.com/anujsit1999/llmgate/ratelimit"
	"github.com/anujsit1999/llmgate/redact"
	"github.com/anujsit1999/llmgate/router"
	"github.com/anujsit1999/llmgate/state"
)

// Gateway is a configured, ready-to-serve gateway.
type Gateway struct {
	cfg       *config.Config
	router    *router.Router
	store     state.Store
	ownsStore bool
	metrics   *metrics.Metrics
	public    http.Handler
	log       *slog.Logger
}

// Option customises New.
type Option func(*options)

type options struct {
	logger    *slog.Logger
	providers map[string]provider.Provider
	store     state.Store
}

// WithLogger sets the logger (default: JSON to stdout).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithProvider supplies a ready-made provider instance under name. Models
// can target it without a matching entry in cfg.Providers; an entry with
// the same name is replaced.
func WithProvider(name string, p provider.Provider) Option {
	return func(o *options) { o.providers[name] = p }
}

// WithStore supplies the counter store instead of building one from
// cfg.State (useful to share one Redis client across components). The
// caller keeps ownership and must close it.
func WithStore(s state.Store) Option { return func(o *options) { o.store = s } }

// New builds a gateway from cfg. cfg is validated; the caller must not
// mutate it afterwards.
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*Gateway, error) {
	o := options{providers: map[string]provider.Provider{}}
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}

	// Injected providers satisfy model references without a config entry.
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.Provider{}
	}
	for name := range o.providers {
		cfg.Providers[name] = config.Provider{Type: "injected"}
	}
	if err := cfg.Prepare(); err != nil {
		return nil, err
	}

	g := &Gateway{cfg: cfg, log: o.logger, store: o.store, metrics: metrics.New()}
	if g.store == nil {
		s, err := state.New(ctx, cfg.State)
		if err != nil {
			return nil, err
		}
		g.store, g.ownsStore = s, true
	}

	providers, err := buildProviders(ctx, cfg, o.providers)
	if err != nil {
		g.Close()
		return nil, err
	}
	ropts := []router.Option{
		router.WithStore(g.store),
		router.WithStateErrorHandler(func(err error) {
			g.log.Warn("state store error; failing open", "error", err.Error())
		}),
	}
	if !cfg.Features.CircuitBreakerOn() {
		ropts = append(ropts, router.WithoutCircuitBreaker())
	}
	if g.router, err = router.NewWithProviders(cfg.Models, providers, ropts...); err != nil {
		g.Close()
		return nil, err
	}

	var red *redact.Redactor
	if cfg.Redaction.Enabled {
		if red, err = redact.New(cfg.Redaction.Types); err != nil {
			g.Close()
			return nil, err
		}
	}
	deps := server.Deps{
		Config: cfg.Server, Redactor: red, Router: g.router, Metrics: g.metrics,
		Logger: g.log, UsageLog: cfg.Features.UsageLogOn(),
	}
	if cfg.Features.AuthOn() {
		deps.Auth = auth.New(cfg.Keys)
	} else {
		g.log.Warn("authentication is DISABLED: every caller has full access; only run behind your own auth layer")
	}
	if cfg.Features.RateLimitOn() {
		deps.Limiter = ratelimit.New(g.store)
	}
	g.public = server.New(deps).Handler()
	g.registerUpstreamMetrics()
	return g, nil
}

// buildProviders constructs the providers models reference, preferring
// injected instances.
func buildProviders(ctx context.Context, cfg *config.Config, injected map[string]provider.Provider) (map[string]provider.Provider, error) {
	out := map[string]provider.Provider{}
	for _, m := range cfg.Models {
		for _, t := range m.Targets {
			if _, done := out[t.Provider]; done {
				continue
			}
			if p, ok := injected[t.Provider]; ok {
				out[t.Provider] = p
				continue
			}
			p, err := provider.Build(ctx, t.Provider, cfg.Providers[t.Provider])
			if err != nil {
				return nil, err
			}
			out[t.Provider] = p
		}
	}
	return out, nil
}

func (g *Gateway) registerUpstreamMetrics() {
	states := map[string]float64{"closed": 0, "open": 1, "half_open": 2}
	g.metrics.RegisterUpstreams(func() []metrics.Upstream {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var out []metrics.Upstream
		for _, u := range g.router.Upstreams(ctx) {
			out = append(out, metrics.Upstream{Deployment: u.Deployment, CircuitState: states[u.State],
				InFlight: float64(u.InFlight), TokensPerMin: u.TokensPerMin})
		}
		return out
	})
}

// Handler serves the public OpenAI-compatible API (/v1/..., /healthz).
func (g *Gateway) Handler() http.Handler { return g.public }

// AdminHandler serves /readyz plus, when enabled, /metrics and /upstreams.
// Expose it only on an internal network.
func (g *Gateway) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ready")) })
	if g.cfg.Features.MetricsOn() {
		mux.Handle("GET /metrics", g.metrics.Handler())
	}
	if g.cfg.Features.AdminAPIOn() {
		mux.HandleFunc("GET /upstreams", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(g.router.Upstreams(r.Context()))
		})
	}
	return mux
}

// Router exposes the routing engine (health, queue depth, direct calls).
func (g *Gateway) Router() *router.Router { return g.router }

// Close releases resources the gateway created (e.g. its Redis connection).
func (g *Gateway) Close() error {
	if g.ownsStore && g.store != nil {
		return g.store.Close()
	}
	return nil
}

// Serve runs the public and admin HTTP servers until ctx is cancelled, then
// shuts down gracefully. It is what cmd/llmgate uses.
func (g *Gateway) Serve(ctx context.Context) error {
	sc := g.cfg.Server
	public := &http.Server{
		Addr:              sc.Addr,
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      sc.RequestTimeout + 10*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	admin := &http.Server{Addr: sc.AdminAddr, Handler: g.AdminHandler(), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	for _, s := range []*http.Server{public, admin} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("listen %s: %w", s.Addr, err)
			}
		}()
	}
	g.log.Info("llmgate listening", "addr", sc.Addr, "admin_addr", sc.AdminAddr,
		"models", g.router.Models(), "state", g.cfg.State.Backend)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	g.log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(public.Shutdown(shutdownCtx), admin.Shutdown(shutdownCtx))
}
