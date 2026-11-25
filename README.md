# llmgate

**One secure, OpenAI-compatible API in front of every LLM provider. Run it as a service or embed it as a Go library.**

llmgate is a production-minded LLM gateway written in Go. Your apps talk to
one endpoint, with one key format and one response shape. llmgate routes
each request across Anthropic, Amazon Bedrock, OpenAI, Azure OpenAI, Llama
via Ollama, or any OpenAI-compatible vendor. It load-balances,
circuit-breaks, falls back, queues, rate-limits, redacts and meters cost
along the way. Every feature ships enabled and can be switched off.

```
            ┌───────────────────────────────── llmgate ───────────────────────────────────┐
 client ──► │ auth → rate limit → validate → redact → router                              │
 (any       │                                           │ strategy: priority│rr│weighted│  │
 OpenAI     │                                           │   least_busy│usage_based (tpm)  │
 SDK)       │                                           │ circuit breaker · capacity      │
            │                                           │ fallback models · bounded queue │
            │    ┌───────────────── provider registry ──▼──────────────────────┐          │
            │    │ anthropic │ bedrock │ openai │ azure_openai │ ollama │        │          │
            │    │ openai_compatible │ mock │ your own (one file)              │          │
            │    └─────────────────────────────────────────────────────────────┘          │
            │ state: memory │ redis (shared limits across replicas)                       │
            │ usage log (no prompts) · Prometheus · /upstreams health                     │
            └─────────────────────────────────────────────────────────────────────────────┘
```

## Features

| | |
|---|---|
| **One response format** | Every provider is normalized to OpenAI Chat Completions, streaming included. Existing SDKs work unchanged. |
| **Pluggable providers** | Adapters register themselves. A new vendor is one file, or no code at all if it's OpenAI-compatible ([guide](docs/adding-a-provider.md)). |
| **Load balancing** | `priority`, `round_robin`, `weighted`, `least_busy` and token-aware `usage_based` strategies across many deployments per model ([routing](docs/routing.md)). |
| **Resilience** | Per-deployment circuit breaker (N failures within a window opens it, then cooldown, half-open probe, exponential backoff). Retries on the next target. Cross-model fallbacks. |
| **Capacity-aware** | Per-deployment `tpm` / `rpm` quotas with sliding windows. Over-quota targets are skipped, and an optional bounded queue absorbs bursts. |
| **Scales out** | `memory` or `redis` state backend (Redis, Valkey, ElastiCache, Cluster), so limits and quotas hold across replicas. If Redis fails, requests keep flowing. |
| **Model-specific params** | Per-target `params` and a client-param allowlist. Gateway-owned fields are protected. |
| **Security** | Hashed keys, per-key model scopes, PII and secret redaction, size and token caps, no prompt logging, distroless non-root image. |
| **Configurable everywhere** | YAML with `${ENV:-default}`, `LLMGATE_*` env vars, CLI flags. Feature switches for auth, rate limits, breaker, metrics, usage log and admin API. |
| **Embeddable** | `llmgate.New(cfg, opts...)` gives you an `http.Handler`. The lower-level packages work standalone. |

## Quickstart (no provider account needed)

```bash
git clone https://github.com/anujsit1999/llmgate && cd llmgate
cp config.example.yaml config.yaml
go run ./cmd/llmgate-keygen -id me -tenant dev   # prints a key + a config snippet
```

Replace the `keys:` entry in `config.yaml` with the snippet, and add
`models: [demo]` to it so the key uses the offline mock provider. Then run:

```bash
go run ./cmd/llmgate --config config.yaml --log-format text
```

```bash
curl -s localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $LLMGATE_KEY" -H "Content-Type: application/json" \
  -d '{"model":"demo","messages":[{"role":"user","content":"my email is jane@example.com"}]}'
```

The mock echoes what it received, so you can see redaction working
(`[REDACTED_EMAIL]`). Add `"stream": true` for server-sent events. Check
deployment health with `curl localhost:9090/upstreams`.

For real models, export provider credentials (for example `ANTHROPIC_API_KEY`)
and allow your key to use `smart` or `fast`.

**Multiple replicas with shared limits:**

```bash
docker compose up --build --scale llmgate=2
```

## Configuration

Three layers, from lowest to highest precedence:

1. **YAML** ([`config.example.yaml`](config.example.yaml) documents every option), with `${VAR}` / `${VAR:-default}` expansion.
2. **Environment variables:** `LLMGATE_ADDR`, `LLMGATE_STATE_BACKEND`, `LLMGATE_REDIS_ADDR`, `LLMGATE_AUTH`, `LLMGATE_RATE_LIMIT`, `LLMGATE_CIRCUIT_BREAKER`, `LLMGATE_METRICS`, `LLMGATE_USAGE_LOG`, `LLMGATE_ADMIN_API`, `LLMGATE_REDACTION`, `LLMGATE_LOG_LEVEL`, `LLMGATE_LOG_FORMAT`, ...
3. **Flags:** the same names, for example `--state-backend redis --rate-limit=false`. Run `llmgate -h` for the full list.

Secrets are never values in config. Fields like `api_key_env` and
`password_env` name the environment variable to read them from.

## Use it as a Go library

```go
import (
    "github.com/anujsit1999/llmgate"
    "github.com/anujsit1999/llmgate/config"
)

cfg, _ := config.Load("config.yaml")            // or build a config.Config in code
gw, err := llmgate.New(ctx, cfg,
    llmgate.WithLogger(logger),
    llmgate.WithProvider("inhouse", myProvider), // any provider.Provider implementation
)
if err != nil { return err }
defer gw.Close()

mux.Handle("/llm/", http.StripPrefix("/llm", gw.Handler()))  // public API
adminMux.Handle("/", gw.AdminHandler())                       // readyz, metrics, upstreams
```

The lower-level packages work on their own too: `provider` (adapters and
registry), `router` (strategies, breaker, fallback), `state` (sliding-window
counters), `redact`, `auth`, `ratelimit` and `config`.

## API

| Endpoint | Description |
|---|---|
| `POST /v1/chat/completions` | OpenAI-compatible chat, streaming and non-streaming. `max_tokens` / `max_completion_tokens`, `temperature`, `reasoning_effort` (`low`…`max`, mapped per provider), plus model-specific extras. |
| `GET /v1/models` | Models this key may use. |
| `GET /healthz` | Liveness. |
| `GET /readyz`, `/metrics`, `/upstreams` | Admin port only (default `127.0.0.1:9090`). |

Errors use OpenAI's envelope. When nothing healthy is available the gateway
returns `503 no_healthy_upstream` with `Retry-After`.

**Provider notes:** current Claude models reject sampling parameters, so
`temperature` is dropped for Anthropic and Bedrock targets; use
`reasoning_effort` instead. `server_side_fallback: true` turns on Anthropic's
server-side refusal fallback for that target.

## Project layout

```
llmgate.go           embeddable Gateway (New, Handler, AdminHandler, Serve)
cmd/llmgate/         binary: flags/env/YAML layering, signals
cmd/llmgate-keygen/  generate client keys
config/              schema, defaults, validation, ${ENV} expansion
provider/            Provider interface, registry, adapters
router/              strategies, circuit breaker, capacity, fallback, queue
state/               sliding-window counters: memory, redis (+ registry)
auth/ ratelimit/ redact/
internal/server/     HTTP handlers, middleware, OpenAI wire types
internal/metrics/    Prometheus collectors
```

## Development

```bash
make test     # unit tests with the race detector (Redis tests use in-process miniredis)
make lint     # golangci-lint (gosec, errorlint, bodyclose, ...)
make vuln     # govulncheck
make docker   # distroless image
```

## Roadmap

- [ ] Tool / function calling pass-through
- [ ] Response cache (Redis) and per-tenant spend budgets
- [ ] OpenTelemetry tracing
- [ ] Prompt-injection heuristics and output guardrails
- [ ] Embeddings and rerank endpoints (for the RAG service in the next phase)
- [ ] Helm chart and Terraform for dev, staging and prod

## Security

See [SECURITY.md](SECURITY.md) and the [threat model](docs/threat-model.md).

## License

Apache-2.0
