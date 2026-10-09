# Project: Courier (Go package mirror on Kubernetes)

Stack: Go, container image (multi-stage Dockerfile), Helm chart in `charts/`.

## Model/cost policy
- Main session (Sonnet) writes and edits all code.
- Delegate read-only and verification work to the Haiku subagents: `code-explorer`, `test-runner`, `security-auditor`, `deploy-checker`.
- Use Opus subagents sparingly: `code-reviewer` (finished feature / pre-release) and `debugger` (only when the cause is unclear after a first attempt).

## Conventions
- Standard Go layout: `cmd/<app>/main.go`, `internal/...`.
- Config via environment variables; log to stdout; expose `/healthz` and `/readyz`; handle SIGTERM with graceful shutdown.
- Run as non-root; pinned image tags; resource requests/limits and probes in the chart.
- Never run cluster-mutating commands without explicit user confirmation.

## Skills
- `/go-review [deep]`: tests + security scan (+ Opus review)
- `/go-deploy <env>`: pre-deploy checks and guarded deploy

## Toolchain (this machine)
- Go 1.27.2 in `~/sdk/go1.27.2`, helm in `~/sdk/helm-4.3.0`, scanners in `~/go/bin`; all symlinked into `~/.local/bin`.
- No `make`, `gcc` or container runtime installed: run the Makefile targets' commands directly; `go test -race` is unavailable without gcc.
- Module name: `courier`. Layout: `cmd/courier`, `internal/{config,observability,server,syncer,provider,storage,fetcher,state}`, `charts/courier`, `deploy/Dockerfile`.
- Opt-in real-data tests: `COURIER_REAL_ALPINE=<dir>` (provider/apk) and `COURIER_LIVE_KEYS=<keys dir>` (syncer, downloads ~300 MB). Keys/fixtures are in `~/dl-alpine`.
- Design: `docs/architecture.md`. Example config: `config.example.yaml`.
