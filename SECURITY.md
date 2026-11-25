# Security Policy

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private vulnerability
reporting instead (Security tab, then "Report a vulnerability"). We aim to
acknowledge reports within 3 business days.

## Supported versions

Only the latest release receives security fixes until 1.0.

## Security design

| Control | Implementation |
|---|---|
| Client authentication | Random 256-bit keys with an `lg_` prefix, so secret scanners can detect them. The gateway stores only SHA-256 hashes. |
| Authorization | Per-key model allowlists. `/v1/models` lists only the models a key may use. |
| Abuse limits | Per-key sliding-window rate limits (shared across replicas with Redis), request body size cap, message count cap, `max_tokens` cap, and an upstream timeout. |
| Data minimization | Prompts and completions are never logged. Regex redaction of emails, card numbers (Luhn-checked), SSNs, phone numbers and cloud or API keys happens before data leaves the gateway. |
| Secrets | Provider credentials come only from environment variables or cloud credential chains, never from config files. `config.yaml` and `.env` are git-ignored. |
| Parameter safety | Gateway-owned fields (`model`, `messages`, `stream`, `max_tokens`, `system`) can't be overridden by clients or config params. Extra parameter names are validated. |
| Error hygiene | Upstream error bodies are logged, never returned to clients. Request IDs are validated before they're echoed. |
| Network | Metrics and readiness are on a separate admin listener bound to localhost by default. Server timeouts guard against slowloris attacks. |
| Supply chain | Minimal dependencies. govulncheck, CodeQL, Dependabot and a Trivy image scan run in CI. |
| Runtime | Distroless static image, non-root user, read-only filesystem, all Linux capabilities dropped (see `docker-compose.yml`). |

## Known limitations

- Regex redaction is best-effort. It won't catch names, addresses or unusual formats.
- With `state.backend: memory` each replica counts separately; use `redis` for multi-replica deployments. If Redis is unreachable, limits fail open (requests continue, a warning is logged).
- TLS termination is expected at your load balancer or ingress.
