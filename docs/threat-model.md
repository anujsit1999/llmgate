# Threat model (STRIDE)

**Assets:** provider credentials, client API keys, prompt and completion
content (which may contain PII), provider spend, and availability.

**Trust boundaries:** client to gateway (untrusted input), gateway to provider
(third party), operator to gateway (config and environment).

| Threat | Example | Mitigation | Status |
|---|---|---|---|
| **S**poofing | Stolen or guessed client key | 256-bit random keys, hashed at rest, `lg_` prefix for secret scanning, per-key scopes | ✅ |
| **T**ampering | Client overrides `model` or `max_tokens` through extra params to bypass routing or caps | Reserved-field list enforced for client and config params; key-name validation | ✅ |
| **T**ampering | Prompt injection makes the model reveal system prompts or misuse tools | Out of scope for v0.1; guardrails are on the roadmap. No tool execution in the gateway. | ⏳ |
| **R**epudiation | Tenant disputes usage | Structured usage log per request: request ID, key ID, tenant, tokens, cost | ✅ |
| **I**nfo disclosure | PII sent to a third-party provider | Redaction before egress; route sensitive tenants to self-hosted models (Ollama/vLLM) | ✅ best-effort |
| **I**nfo disclosure | Prompts leak through logs | Prompts and completions are never logged | ✅ |
| **I**nfo disclosure | Upstream error reveals provider internals or keys | Upstream bodies are logged server-side only; clients get generic errors | ✅ |
| **I**nfo disclosure | Metrics endpoint exposed publicly | Separate admin listener, localhost by default | ✅ |
| **D**enial of service | Huge bodies, slowloris, token-burning requests | Body cap, header and read timeouts, `max_tokens` cap, rate limits, upstream timeout | ✅ |
| **D**enial of service | Cost exhaustion ("denial of wallet") | Cluster-wide rate limits and per-deployment tpm/rpm caps plus cost metrics; enforced tenant budgets on the roadmap | ⏳ |
| **D**enial of service | Failing provider drags down latency | Circuit breaker skips failing deployments; fallback models; bounded queue with max_depth | ✅ |
| **E**levation | Scoped key uses an expensive model | Per-key model allowlist checked on every request | ✅ |
| **E**levation | Container breakout | Distroless, non-root, read-only filesystem, no capabilities, no-new-privileges | ✅ |

Re-review this document whenever a feature crosses a trust boundary (for
example tool calling, caching, or a new provider type).
