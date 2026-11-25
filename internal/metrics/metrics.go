// Package metrics exposes Prometheus metrics on a dedicated registry.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics groups every collector the gateway records.
type Metrics struct {
	Registry    *prometheus.Registry
	Requests    *prometheus.CounterVec
	Latency     *prometheus.HistogramVec
	Tokens      *prometheus.CounterVec
	CostUSD     *prometheus.CounterVec
	Redactions  *prometheus.CounterVec
	RateLimited *prometheus.CounterVec
	Fallbacks   *prometheus.CounterVec
	Skipped     *prometheus.CounterVec
}

// Upstream is one deployment's live state, for gauges.
type Upstream struct {
	Deployment   string
	CircuitState float64 // 0 closed, 1 open, 2 half-open
	InFlight     float64
	TokensPerMin float64
}

type upstreamCollector struct {
	fn                     func() []Upstream
	state, inflight, tpmGa *prometheus.Desc
}

func (c *upstreamCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.inflight
	ch <- c.tpmGa
}

func (c *upstreamCollector) Collect(ch chan<- prometheus.Metric) {
	for _, u := range c.fn() {
		ch <- prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, u.CircuitState, u.Deployment)
		ch <- prometheus.MustNewConstMetric(c.inflight, prometheus.GaugeValue, u.InFlight, u.Deployment)
		ch <- prometheus.MustNewConstMetric(c.tpmGa, prometheus.GaugeValue, u.TokensPerMin, u.Deployment)
	}
}

// RegisterUpstreams exposes per-deployment circuit state and load gauges,
// read from fn at scrape time.
func (m *Metrics) RegisterUpstreams(fn func() []Upstream) {
	l := []string{"deployment"}
	m.Registry.MustRegister(&upstreamCollector{
		fn:       fn,
		state:    prometheus.NewDesc("llmgate_circuit_state", "Circuit breaker state (0 closed, 1 open, 2 half-open).", l, nil),
		inflight: prometheus.NewDesc("llmgate_upstream_in_flight", "Requests in flight per deployment.", l, nil),
		tpmGa:    prometheus.NewDesc("llmgate_upstream_tokens_per_minute", "Sliding-window tokens per minute per deployment.", l, nil),
	})
}

// New registers all collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_requests_total", Help: "Chat requests by model, provider and outcome.",
		}, []string{"model", "provider", "status"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "llmgate_upstream_duration_seconds", Help: "Upstream call duration.",
			Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "provider"}),
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_tokens_total", Help: "Tokens processed, by tenant, model and direction.",
		}, []string{"tenant", "model", "direction"}),
		CostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_cost_usd_total", Help: "Estimated spend in USD.",
		}, []string{"tenant", "model"}),
		Redactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_redactions_total", Help: "Sensitive values redacted from prompts.",
		}, []string{"type"}),
		RateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_rate_limited_total", Help: "Requests rejected by rate limiting.",
		}, []string{"tenant"}),
		Fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_fallbacks_total", Help: "Times a request fell back to a later target.",
		}, []string{"model"}),
		Skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llmgate_targets_skipped_total", Help: "Targets skipped because their circuit was open or they were over capacity.",
		}, []string{"model", "deployment", "reason"}),
	}
	reg.MustRegister(m.Requests, m.Latency, m.Tokens, m.CostUSD, m.Redactions, m.RateLimited, m.Fallbacks, m.Skipped,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// Handler serves /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}
