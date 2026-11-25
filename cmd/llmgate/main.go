// Command llmgate runs the LLM gateway.
//
// Configuration layers, lowest to highest precedence (TEI-style: every
// deployment knob is reachable without editing files):
//
//  1. the YAML file (--config / LLMGATE_CONFIG), with ${VAR:-default} expansion
//  2. LLMGATE_* environment variables
//  3. command-line flags
//
// Run `llmgate -h` for the full list.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/anujsit1999/llmgate"
	"github.com/anujsit1999/llmgate/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "llmgate:", err)
		os.Exit(1)
	}
}

// override is one setting reachable as a flag and an env var.
type override struct {
	flag, env, usage string
	apply            func(*config.Config, string) error
}

func boolPtr(v string) (*bool, error) {
	b, err := strconv.ParseBool(v)
	return &b, err
}

var overrides = []override{
	{"addr", "LLMGATE_ADDR", "public listen address", func(c *config.Config, v string) error { c.Server.Addr = v; return nil }},
	{"admin-addr", "LLMGATE_ADMIN_ADDR", "admin listen address (/metrics, /readyz, /upstreams)", func(c *config.Config, v string) error { c.Server.AdminAddr = v; return nil }},
	{"state-backend", "LLMGATE_STATE_BACKEND", "counter store: memory | redis", func(c *config.Config, v string) error { c.State.Backend = v; return nil }},
	{"redis-addr", "LLMGATE_REDIS_ADDR", "redis address(es), comma-separated for cluster", func(c *config.Config, v string) error { c.State.Redis.Addr = v; return nil }},
	{"redaction", "LLMGATE_REDACTION", "enable PII/secret redaction (true|false)", func(c *config.Config, v string) error {
		b, err := strconv.ParseBool(v)
		c.Redaction.Enabled = b
		return err
	}},
	{"auth", "LLMGATE_AUTH", "enable API-key auth (true|false)", func(c *config.Config, v string) (err error) { c.Features.Auth, err = boolPtr(v); return }},
	{"rate-limit", "LLMGATE_RATE_LIMIT", "enable per-key rate limits (true|false)", func(c *config.Config, v string) (err error) { c.Features.RateLimit, err = boolPtr(v); return }},
	{"circuit-breaker", "LLMGATE_CIRCUIT_BREAKER", "enable circuit breaking (true|false)", func(c *config.Config, v string) (err error) { c.Features.CircuitBreaker, err = boolPtr(v); return }},
	{"metrics", "LLMGATE_METRICS", "enable /metrics (true|false)", func(c *config.Config, v string) (err error) { c.Features.Metrics, err = boolPtr(v); return }},
	{"usage-log", "LLMGATE_USAGE_LOG", "enable per-request usage logs (true|false)", func(c *config.Config, v string) (err error) { c.Features.UsageLog, err = boolPtr(v); return }},
	{"admin-api", "LLMGATE_ADMIN_API", "enable /upstreams (true|false)", func(c *config.Config, v string) (err error) { c.Features.AdminAPI, err = boolPtr(v); return }},
}

func run() error {
	fs := flag.NewFlagSet("llmgate", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LLMGATE_CONFIG", "config.yaml"), "config file [LLMGATE_CONFIG]")
	logLevel := fs.String("log-level", envOr("LLMGATE_LOG_LEVEL", "info"), "debug | info | warn | error [LLMGATE_LOG_LEVEL]")
	logFormat := fs.String("log-format", envOr("LLMGATE_LOG_FORMAT", "json"), "json | text [LLMGATE_LOG_FORMAT]")
	showVersion := fs.Bool("version", false, "print version and exit")
	flagVals := make([]*string, len(overrides))
	for i, o := range overrides {
		flagVals[i] = fs.String(o.flag, "", o.usage+" ["+o.env+"]")
	}
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("llmgate", version)
		return nil
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// env first, then flags, so flags win.
	for i, o := range overrides {
		for _, v := range []string{os.Getenv(o.env), *flagVals[i]} {
			if v == "" {
				continue
			}
			if err := o.apply(cfg, v); err != nil {
				return fmt.Errorf("--%s / %s: %w", o.flag, o.env, err)
			}
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gw, err := llmgate.New(ctx, cfg, llmgate.WithLogger(log))
	if err != nil {
		return err
	}
	defer gw.Close()
	log.Info("llmgate starting", "version", version)
	return gw.Serve(ctx)
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q", format)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
