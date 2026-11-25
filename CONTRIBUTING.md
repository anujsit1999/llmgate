# Contributing

1. Fork the repo and create a branch: `git checkout -b feat/my-change`.
2. `make test lint`. CI runs the same checks, plus govulncheck, CodeQL and Trivy.
3. Keep pull requests focused, add tests for behavior changes, and update docs or config examples.
4. Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, ...).

**Adding a provider?** See [docs/adding-a-provider.md](docs/adding-a-provider.md).
**Security issue?** Follow [SECURITY.md](SECURITY.md). Please don't open a public issue.
