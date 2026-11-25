// Package config loads and validates llmgate's YAML configuration.
//
// Secrets never live in the config file: provider API keys are read from the
// environment variables named by `api_key_env`, and client API keys are stored
// only as SHA-256 hashes.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration document.
type Config struct {
	Server    Server              `yaml:"server"`
	Providers map[string]Provider `yaml:"providers"`
	Models    []Model             `yaml:"models"`
	Keys      []Key               `yaml:"keys"`
	Redaction Redaction           `yaml:"redaction"`
	// CircuitBreaker is the default for every model; models may override it.
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	// State selects where shared counters live (rate limits, usage windows).
	State State `yaml:"state"`
	// Features switches whole subsystems on or off. Everything defaults to on.
	Features Features `yaml:"features"`
}

// Features toggles gateway subsystems. A nil value means enabled; use
// the On helpers to read them. (Redaction has its own redaction.enabled;
// queues, fallbacks and param pass-through are enabled per model by
// configuring them.)
type Features struct {
	Auth           *bool `yaml:"auth"`            // API-key auth; off = anonymous access (only behind your own auth!)
	RateLimit      *bool `yaml:"rate_limit"`      // per-key rpm limits
	CircuitBreaker *bool `yaml:"circuit_breaker"` // trip failing deployments out of rotation
	Metrics        *bool `yaml:"metrics"`         // Prometheus /metrics on the admin port
	UsageLog       *bool `yaml:"usage_log"`       // one structured log line per request
	AdminAPI       *bool `yaml:"admin_api"`       // /upstreams status on the admin port
}

func on(b *bool) bool { return b == nil || *b }

// AuthOn reports whether API-key authentication is enabled.
func (f Features) AuthOn() bool { return on(f.Auth) }

// RateLimitOn reports whether per-key rate limiting is enabled.
func (f Features) RateLimitOn() bool { return on(f.RateLimit) }

// CircuitBreakerOn reports whether circuit breaking is enabled.
func (f Features) CircuitBreakerOn() bool { return on(f.CircuitBreaker) }

// MetricsOn reports whether the metrics endpoint is enabled.
func (f Features) MetricsOn() bool { return on(f.Metrics) }

// UsageLogOn reports whether per-request usage logging is enabled.
func (f Features) UsageLogOn() bool { return on(f.UsageLog) }

// AdminAPIOn reports whether the upstream status endpoint is enabled.
func (f Features) AdminAPIOn() bool { return on(f.AdminAPI) }

// State selects the counter backend. Use "redis" when running more than one
// replica so limits and capacity are enforced across all of them.
type State struct {
	Backend string `yaml:"backend"` // memory (default) | redis
	Redis   Redis  `yaml:"redis"`
}

// Redis connection settings. Addr may be a comma-separated list for
// Redis Cluster; credentials come from the environment.
type Redis struct {
	Addr        string `yaml:"addr"`
	PasswordEnv string `yaml:"password_env"`
	DB          int    `yaml:"db"`
	TLS         bool   `yaml:"tls"`
	KeyPrefix   string `yaml:"key_prefix"`
}

// Addrs splits Addr into a list.
func (r Redis) Addrs() []string {
	var out []string
	for _, a := range strings.Split(r.Addr, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Password resolves the Redis password from the environment.
func (r Redis) Password() string {
	if r.PasswordEnv == "" {
		return ""
	}
	return os.Getenv(r.PasswordEnv)
}

// Queue lets requests wait briefly when every deployment of a model is at
// its tpm/rpm capacity, instead of failing immediately. It never applies to
// outages (open circuits): those fail over or fail fast.
type Queue struct {
	MaxWait  time.Duration `yaml:"max_wait"`  // longest a request may wait
	MaxDepth int           `yaml:"max_depth"` // waiting requests per model (per replica)
}

// CircuitBreaker trips a deployment out of rotation after FailureThreshold
// retryable failures within Window, for Cooldown (doubling on each failed
// probe, up to MaxCooldown).
type CircuitBreaker struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	Window           time.Duration `yaml:"window"`
	Cooldown         time.Duration `yaml:"cooldown"`
	MaxCooldown      time.Duration `yaml:"max_cooldown"`
}

func (cb *CircuitBreaker) applyDefaults(base CircuitBreaker) {
	if cb.FailureThreshold == 0 {
		cb.FailureThreshold = base.FailureThreshold
	}
	if cb.Window == 0 {
		cb.Window = base.Window
	}
	if cb.Cooldown == 0 {
		cb.Cooldown = base.Cooldown
	}
	if cb.MaxCooldown == 0 {
		cb.MaxCooldown = base.MaxCooldown
	}
}

// DefaultCircuitBreaker: 5 failures in 5 minutes opens the circuit for 30s.
var DefaultCircuitBreaker = CircuitBreaker{
	FailureThreshold: 5, Window: 5 * time.Minute, Cooldown: 30 * time.Second, MaxCooldown: 5 * time.Minute,
}

// Routing strategies for spreading load across a model's targets.
const (
	StrategyPriority   = "priority"    // first healthy target in list order (failover)
	StrategyRoundRobin = "round_robin" // rotate evenly
	StrategyWeighted   = "weighted"    // smooth weighted round-robin by `weight`
	StrategyLeastBusy  = "least_busy"  // fewest requests in flight
	StrategyUsageBased = "usage_based" // lowest tokens/requests-per-minute utilisation vs tpm/rpm
)

var validStrategies = map[string]bool{
	StrategyPriority: true, StrategyRoundRobin: true, StrategyWeighted: true,
	StrategyLeastBusy: true, StrategyUsageBased: true,
}

// Server holds listener and request-limit settings.
type Server struct {
	Addr             string        `yaml:"addr"`
	AdminAddr        string        `yaml:"admin_addr"`
	MaxBodyBytes     int64         `yaml:"max_body_bytes"`
	RequestTimeout   time.Duration `yaml:"request_timeout"`
	DefaultMaxTokens int           `yaml:"default_max_tokens"`
	MaxTokensCap     int           `yaml:"max_tokens_cap"`
	MaxMessages      int           `yaml:"max_messages"`
}

// Provider describes one upstream LLM provider. Which fields matter depends
// on Type; valid types are whatever adapters registered themselves in
// internal/provider (anthropic, bedrock, openai, azure_openai, ollama,
// openai_compatible, mock, ...).
type Provider struct {
	Type      string            `yaml:"type"`
	BaseURL   string            `yaml:"base_url"`
	APIKeyEnv string            `yaml:"api_key_env"`
	Region    string            `yaml:"region"`  // cloud region (bedrock)
	Headers   map[string]string `yaml:"headers"` // extra static headers (non-secret)
}

// APIKey resolves the provider's secret from the environment.
func (p Provider) APIKey() string {
	if p.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(p.APIKeyEnv)
}

// Model is a client-facing model alias backed by one or more deployments
// (targets). Strategy picks the order targets are tried in; unhealthy or
// over-capacity targets are skipped; Fallbacks name other models to try
// when every target of this one is unavailable or failing.
type Model struct {
	Name           string          `yaml:"name"`
	Strategy       string          `yaml:"strategy"`
	Targets        []Target        `yaml:"targets"`
	Fallbacks      []string        `yaml:"fallbacks"`
	CircuitBreaker *CircuitBreaker `yaml:"circuit_breaker"`
	Queue          *Queue          `yaml:"queue"` // nil = no queueing
}

// Target is a concrete (provider, upstream model) pair.
type Target struct {
	Provider string  `yaml:"provider"`
	Model    string  `yaml:"model"`
	Pricing  Pricing `yaml:"pricing"`
	// Weight for the weighted strategy (default 1).
	Weight int `yaml:"weight"`
	// TPM / RPM are this deployment's capacity (tokens / requests per minute).
	// When set, the target is skipped once it would exceed them, and the
	// usage_based strategy balances by utilisation. 0 = unlimited.
	TPM int `yaml:"tpm"`
	RPM int `yaml:"rpm"`
	// ServerSideFallback asks Anthropic to re-serve a refused request on a
	// fallback model inside the same call (Anthropic targets only).
	ServerSideFallback bool `yaml:"server_side_fallback"`
	// Params are model-specific body fields merged into every upstream
	// request for this target (e.g. top_p, seed, thinking). They override
	// anything the client sent. Dotted keys set nested fields.
	Params map[string]any `yaml:"params"`
	// ClientParams controls which extra (non-standard) request fields from
	// clients are forwarded: unset or ["*"] = any, [] = none, else an allowlist.
	ClientParams []string `yaml:"client_params"`
}

// AllowsClientParam reports whether a client-supplied extra field may be forwarded.
func (t Target) AllowsClientParam(key string) bool {
	if t.ClientParams == nil {
		return true
	}
	for _, k := range t.ClientParams {
		if k == "*" || k == key {
			return true
		}
	}
	return false
}

// reservedParams are request fields the gateway owns. Neither clients nor
// config params may set them, so routing, token caps and streaming stay
// under the gateway's control.
var reservedParams = map[string]bool{
	"model": true, "messages": true, "stream": true, "stream_options": true,
	"system": true, "max_tokens": true, "max_completion_tokens": true,
}

var validParamKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

// CheckParamKey validates an extra-parameter name.
func CheckParamKey(key string) error {
	root, _, _ := strings.Cut(key, ".")
	if reservedParams[root] {
		return fmt.Errorf("parameter %q is reserved by the gateway", key)
	}
	if !validParamKey.MatchString(key) || len(key) > 64 {
		return fmt.Errorf("invalid parameter name %q", key)
	}
	return nil
}

// Pricing is USD per million tokens, used for cost metering.
type Pricing struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

// Cost returns the USD cost of a request.
func (p Pricing) Cost(inputTokens, outputTokens int) float64 {
	return (float64(inputTokens)*p.InputPerMTok + float64(outputTokens)*p.OutputPerMTok) / 1_000_000
}

// Key is a client API key, stored as a hash.
type Key struct {
	ID     string   `yaml:"id"`
	Tenant string   `yaml:"tenant"`
	Hash   string   `yaml:"hash"` // "sha256:<hex>"
	Models []string `yaml:"models"`
	RPM    int      `yaml:"rpm"`
}

// Redaction controls PII/secret scrubbing of prompts before they leave the gateway.
type Redaction struct {
	Enabled bool     `yaml:"enabled"`
	Types   []string `yaml:"types"`
}

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse expands ${VAR} / ${VAR:-default} placeholders, decodes YAML
// (rejecting unknown fields), applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	expanded, err := ExpandEnv(string(raw))
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Prepare(); err != nil {
		return nil, err
	}
	return &c, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} and ${VAR:-default}. A ${VAR} with no default
// that is unset is an error, so typos fail loudly instead of becoming "".
// Use it for non-secret deployment values (addresses, regions, URLs);
// secrets still belong in *_env fields so they never sit in a config string.
func ExpandEnv(s string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(s, func(m string) string {
		g := envRef.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(g[1]); ok {
			return v
		}
		if strings.Contains(m, ":-") {
			return g[2]
		}
		missing = append(missing, g[1])
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Prepare applies defaults and validates. Call it on a Config built in code
// (library use); Load and Parse call it for you.
func (c *Config) Prepare() error {
	c.applyDefaults()
	return c.Validate()
}

func (c *Config) applyDefaults() {
	s := &c.Server
	if s.Addr == "" {
		s.Addr = ":8080"
	}
	if s.AdminAddr == "" {
		s.AdminAddr = "127.0.0.1:9090"
	}
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	if s.RequestTimeout == 0 {
		s.RequestTimeout = 120 * time.Second
	}
	if s.DefaultMaxTokens == 0 {
		s.DefaultMaxTokens = 4096
	}
	if s.MaxTokensCap == 0 {
		s.MaxTokensCap = 64000
	}
	if s.MaxMessages == 0 {
		s.MaxMessages = 256
	}
	for i := range c.Keys {
		if c.Keys[i].RPM == 0 {
			c.Keys[i].RPM = 60
		}
	}
	c.CircuitBreaker.applyDefaults(DefaultCircuitBreaker)
	if c.State.Backend == "" {
		c.State.Backend = "memory"
	}
	for i := range c.Models {
		m := &c.Models[i]
		if m.Strategy == "" {
			m.Strategy = StrategyPriority
		}
		if m.CircuitBreaker == nil {
			cb := c.CircuitBreaker
			m.CircuitBreaker = &cb
		} else {
			m.CircuitBreaker.applyDefaults(c.CircuitBreaker)
		}
		if q := m.Queue; q != nil {
			if q.MaxWait == 0 {
				q.MaxWait = 10 * time.Second
			}
			if q.MaxDepth == 0 {
				q.MaxDepth = 100
			}
		}
		for j := range m.Targets {
			if m.Targets[j].Weight == 0 {
				m.Targets[j].Weight = 1
			}
		}
	}
}

// Validate checks cross-references and obvious mistakes.
func (c *Config) Validate() error {
	var errs []error
	if c.State.Backend == "redis" && c.State.Redis.Addr == "" {
		errs = append(errs, errors.New("state.redis.addr is required when state.backend is redis"))
	}
	if c.Server.DefaultMaxTokens > c.Server.MaxTokensCap {
		errs = append(errs, errors.New("server.default_max_tokens exceeds server.max_tokens_cap"))
	}
	for name, p := range c.Providers {
		if p.Type == "" {
			errs = append(errs, fmt.Errorf("provider %q: type is required", name))
		}
	}
	seenModel := map[string]bool{}
	for _, m := range c.Models {
		if m.Name == "" {
			errs = append(errs, errors.New("model with empty name"))
			continue
		}
		if seenModel[m.Name] {
			errs = append(errs, fmt.Errorf("model %q defined twice", m.Name))
		}
		seenModel[m.Name] = true
		if len(m.Targets) == 0 {
			errs = append(errs, fmt.Errorf("model %q has no targets", m.Name))
		}
		if q := m.Queue; q != nil && (q.MaxWait < 0 || q.MaxDepth < 0 || q.MaxWait >= c.Server.RequestTimeout) {
			errs = append(errs, fmt.Errorf("model %q: queue.max_wait must be positive and below server.request_timeout", m.Name))
		}
		if !validStrategies[m.Strategy] {
			errs = append(errs, fmt.Errorf("model %q: unknown strategy %q", m.Name, m.Strategy))
		}
		if cb := m.CircuitBreaker; cb != nil && (cb.FailureThreshold < 1 || cb.Window <= 0 || cb.Cooldown <= 0 || cb.MaxCooldown < cb.Cooldown) {
			errs = append(errs, fmt.Errorf("model %q: circuit_breaker needs failure_threshold >= 1, positive window/cooldown, max_cooldown >= cooldown", m.Name))
		}
		for _, t := range m.Targets {
			if t.Weight < 0 || t.TPM < 0 || t.RPM < 0 {
				errs = append(errs, fmt.Errorf("model %q: weight, tpm and rpm must not be negative", m.Name))
			}
			p, ok := c.Providers[t.Provider]
			if !ok {
				errs = append(errs, fmt.Errorf("model %q: unknown provider %q", m.Name, t.Provider))
				continue
			}
			for k := range t.Params {
				if err := CheckParamKey(k); err != nil {
					errs = append(errs, fmt.Errorf("model %q: %w", m.Name, err))
				}
			}
			if t.ServerSideFallback && p.Type != "anthropic" {
				errs = append(errs, fmt.Errorf("model %q: server_side_fallback is only valid for type anthropic", m.Name))
			}
		}
	}
	for _, m := range c.Models {
		for _, f := range m.Fallbacks {
			if f == m.Name {
				errs = append(errs, fmt.Errorf("model %q lists itself as a fallback", m.Name))
			} else if !seenModel[f] {
				errs = append(errs, fmt.Errorf("model %q: unknown fallback model %q", m.Name, f))
			}
		}
	}
	seenKey := map[string]bool{}
	for _, k := range c.Keys {
		if k.ID == "" || k.Tenant == "" {
			errs = append(errs, errors.New("every key needs an id and a tenant"))
		}
		if seenKey[k.ID] {
			errs = append(errs, fmt.Errorf("key id %q defined twice", k.ID))
		}
		seenKey[k.ID] = true
		if !strings.HasPrefix(k.Hash, "sha256:") || len(k.Hash) != len("sha256:")+64 {
			errs = append(errs, fmt.Errorf("key %q: hash must be sha256:<64 hex chars> (use `llmgate-keygen`)", k.ID))
		}
		for _, m := range k.Models {
			if !seenModel[m] {
				errs = append(errs, fmt.Errorf("key %q: unknown model %q", k.ID, m))
			}
		}
	}
	return errors.Join(errs...)
}
