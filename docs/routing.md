# Routing, resilience and scaling

## How a request is routed

```
client asks for "smart"
   │
   ▼
1. ORDER    the model's targets with its strategy
2. FILTER   skip targets whose circuit is open      → "circuit_open"
            skip targets over their tpm/rpm quota   → "over_capacity"
3. EXTEND   append targets of fallback models (smart → fast → ...), deduplicated
4. QUEUE    nothing left and *only* capacity was the problem? wait (bounded)
5. TRY      candidates in order:
              success            → breaker.Success, done
              429 / 5xx / 401 / 403 / 404 / network → breaker.Failure, next candidate
              400 / 413 / 422    → return it (another deployment would reject it too)
              client disconnected → stop, breaker untouched
              output already streamed → stop (can't switch mid-stream)
6. NOTHING WORKED → 503 no_healthy_upstream (with Retry-After) or the last error
```

## Strategies (per model)

| `strategy` | Picks first | Use when |
|---|---|---|
| `priority` (default) | first healthy target in list order | primary + backup (e.g. Anthropic, then Bedrock) |
| `round_robin` | rotates evenly | identical replicas, e.g. several vLLM pods |
| `weighted` | smooth weighted round-robin by `weight` (5:1:1 gives a a b a c a a) | uneven capacity, canary rollouts (weight 95/5) |
| `least_busy` | fewest requests in flight (on this replica) | slow, variable-latency self-hosted models |
| `usage_based` | lowest share of its `tpm`/`rpm` used in the last minute | several cloud deployments with per-deployment quotas (Azure, Bedrock) |

Every strategy only decides who goes **first**. All other healthy targets
stay in the list as failover candidates.

## Capacity and token-aware routing

Set `tpm` and/or `rpm` on a target to its quota. Before each request the
router estimates prompt tokens (about 4 characters per token). It skips a target if
`tokens used in the last 60s + estimate > tpm`, or `requests + 1 > rpm`. After the
call it records the **actual** token usage the provider reported. The window
is a sliding window counter: the current minute plus a weighted share of
the previous one, so it doesn't reset abruptly at the top of each minute.

## Circuit breaker

```
closed ──(failure_threshold failures within window)──► open
  ▲                                                     │ cooldown
  └──── probe succeeds ◄── half_open (exactly 1 probe) ◄┘
                              │ probe fails: cooldown ×2 (≤ max_cooldown)
                              └────────────► open
```

Defaults: 5 failures in 5 minutes opens the circuit for 30 seconds, backing off to at most 5
minutes. Health is tracked per **deployment** (provider + upstream model),
so two aliases sharing a deployment share its health. Check it at
`GET /upstreams` on the admin port, or with `llmgate_circuit_state`.

## Queueing: when it helps

The queue (`queue: {max_wait, max_depth}`) is used **only** when every
candidate was skipped as `over_capacity`. Quotas free up within seconds as
the minute slides, so a short wait beats returning an error. llmgate never
queues during an outage (open circuits): waiting can't fix a dead provider,
so it fails over or fails fast.

llmgate doesn't batch requests the way TEI does. TEI batches because it
runs the model on its own GPU. llmgate proxies remote APIs that batch
internally, and self-hosted servers (vLLM, Ollama) already do continuous
batching. For offline bulk work, use provider batch APIs.

## Running several replicas

| State | Where | Why |
|---|---|---|
| Client rate limits | `state` backend | must be global, or N pods give N× the limit |
| Deployment rpm/tpm windows | `state` backend | must be global, or N pods use N× the provider quota |
| Circuit breakers | each replica | standard practice (as in Envoy outlier detection); no network hop, no shared failure point |
| In-flight counts (`least_busy`) | each replica | a replica balances the concurrency it controls |
| Queue depth | each replica | `max_depth` is per replica |

Use `state.backend: redis` (Redis, Valkey, ElastiCache or Redis Cluster)
for more than one replica. Keys use Redis Cluster hash tags and expire on
their own; the check-and-increment for rate limits is one atomic Lua script.
If Redis becomes unreachable, llmgate **fails open**: requests keep flowing
and a warning is logged, so a cache outage doesn't become an API outage.

```bash
docker compose up --build --scale llmgate=2   # two replicas + Redis
```
