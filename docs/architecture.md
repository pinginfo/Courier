# Courier: Architecture

Courier is a self-hosted multi-ecosystem package mirror for a homelab.

Status: draft v1 (2026-10-09)

## 1. Goals

- One application to mirror multiple package ecosystems into an S3 bucket (MinIO), instead of one tool per distro (debmirror, reposync, ...).
- v1 targets: **Ubuntu (APT)**, **Fedora (RPM)**, **Alpine (APK)**.
- Later: **proxy-cache** for language ecosystems (Go modules, npm, PyPI) and **container images (OCI)**.
- Runs on Kubernetes. MinIO runs **outside** the cluster.
- Everything (distros, releases, architectures, components, schedules) is driven by a **config file**.
- Built-in observability: Prometheus metrics, structured logs, health probes, Grafana dashboard, alert rules.

Non-goals (v1): re-signing repositories, serving package bytes from the app, a web UI, multi-tenancy.

## 2. Key design decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Mirror byte-for-byte, never re-sign.** | Upstream GPG/APK signatures stay valid. Clients trust official keys; no signing key to manage. The app *verifies* signatures while syncing but never alters content. |
| D2 | **Ordered publish: packages first, metadata last, delete after a grace period.** | Metadata references packages by hash. A client updating mid-sync must never see a broken repo. |
| D3 | **The app is a control plane only.** Clients download straight from MinIO. | Keeps the Go app out of the data path; MinIO already serves objects well. |
| D4 | **Two modes: `sync` and `proxy-cache`.** | Distros are mirrored on a schedule (filtered full copy). Language/OCI ecosystems are too large to mirror fully, so they are fetched on first request and cached in S3. v1 builds `sync` only; the provider interface allows both. |
| D5 | **State lives in the bucket** (per-repo manifest), no database in v1. | Pods stay stateless; state sits next to the data. A DB can be added later for history/UI. |
| D6 | **Config-file driven**, mounted from a ConfigMap; secrets from a Secret. | Nothing hardcoded; adding a release is a config change. |
| D7 | **One Go binary**, two commands: `serve` and `sync`. | `serve` = scheduler + API + metrics. `sync <repo>` = one-off run (debugging, or run as a CronJob). |

## 3. System overview

```
                 ┌────────────────── Kubernetes cluster ──────────────────┐
 upstream        │  courier (Deployment, 1 replica)                       │        ┌───────────────┐
 mirrors   ────► │   ├ scheduler (cron per repo)                          │ ─────► │ MinIO (external│
 (HTTPS)         │   ├ sync workers ──► providers: apt | rpm | apk        │  S3 API│ to the cluster)│
                 │   ├ API: /status  POST /sync/{repo}  /healthz /readyz  │        └──────┬────────┘
                 │   └ /metrics   JSON logs (stdout)                      │               │ HTTP(S), read-only
                 └──────┬────────────────────────────────────────────────┘               ▼
                        │ scrape / collect                                    apt / dnf / apk clients
                 Prometheus · Grafana · (log collector)
```

Clients reach the bucket through the MinIO endpoint (anonymous read-only policy on the mirror prefix), optionally behind a reverse proxy for TLS and a friendly hostname. The cluster does not need an ingress for package traffic.

A single replica is enough for v1 (one writer per repo). The scheduler holds a per-repo lock so manual and scheduled runs never overlap.

## 4. Components and package layout

```
cmd/courier/            main: `serve`, `sync <repo>`
internal/config/        load + validate YAML config
internal/provider/      Provider interface + apt/, rpm/, apk/   (later: gomod/, npm/, pypi/, oci/)
internal/syncer/        generic engine: plan -> diff -> download -> upload -> publish -> GC
internal/fetcher/       HTTP client: retries/backoff, conditional GET, concurrency, rate limit, checksum verify
internal/verify/        GPG (APT/RPM) and APK signature verification
internal/storage/       Storage interface (S3 via minio-go) + in-memory fake for tests
internal/state/         per-repo manifest read/write
internal/server/        HTTP API, health, metrics
internal/observability/ slog setup, Prometheus collectors
charts/courier/         Helm chart
deploy/                 Dockerfile, example values
docs/
```

### Provider interface (the extensibility point)

```go
// A Provider understands one repository format. It does not download or upload
// bulk data itself; the engine does that.
type Provider interface {
    // Plan fetches and verifies upstream metadata, applies the repo's filters
    // (release, arch, component...), and returns the files that must exist.
    Plan(ctx context.Context, repo config.Repo) (Plan, error)

    // Publish uploads/switches the metadata files last, in the order the format requires.
    Publish(ctx context.Context, plan Plan) error
}

type Plan struct {
    Packages []File // pool/package files: immutable, content-addressed by checksum
    Metadata []File // index files: uploaded last, in order
}
type File struct { Path string; Size int64; Checksum Checksum }
```

The engine is generic: it diffs `Plan` against the stored manifest and the bucket, downloads only what is missing or changed (resumable: skip objects already present with matching size/checksum), uploads, then calls `Publish`.

### Sync flow

1. **Lock** the repo.
2. **Plan**: fetch metadata from upstream, verify its signature, build the wanted file list after filters.
3. **Diff** against the manifest (and S3 `HEAD` for safety).
4. **Download + verify checksum**, upload packages to S3 with bounded concurrency.
5. **Publish** metadata last (APT: `by-hash` entries, then `Packages`, then `InRelease`/`Release`; RPM: `repodata/*` then `repomd.xml` last; APK: `APKINDEX.tar.gz` last).
6. **Write manifest** (new snapshot of what is published).
7. **GC**: delete objects not referenced by the manifest, only after `retention.grace_period`.
8. **Unlock**, emit metrics/logs.

### Format notes

- **APT (Ubuntu):** `dists/<suite>/{InRelease,Release,Release.gpg}`, `Packages.*`, `by-hash/`, shared `pool/`. Filter by suite (`noble`, `noble-updates`, `noble-security`), component, architecture. Optional: `Contents`, `i18n`, `dep11` (off by default).
- **RPM (Fedora):** `repodata/repomd.xml` (+ signature) referencing `primary/filelists/other`; packages under `Packages/`. Filter by release, arch, repo (`releases`, `updates`, `updates-testing`), optional modular metadata.
- **APK (Alpine):** `<branch>/<repo>/<arch>/APKINDEX.tar.gz` (signed) + `*.apk`. Filter by branch (`v3.20`; `edge` is opt-in), repo (`main`, `community`), arch.

## 5. Configuration

One YAML file, validated at startup (`courier validate -c config.yaml` for CI). Secrets are referenced by env var, never inline.

```yaml
storage:
  endpoint: minio.lan:9000          # external to the cluster
  bucket: mirror
  region: us-east-1
  use_ssl: true
  path_style: true                  # MinIO
  ca_file: /etc/courier/ca.crt      # optional, for a private CA
  access_key_env: S3_ACCESS_KEY     # read from env (Kubernetes Secret)
  secret_key_env: S3_SECRET_KEY

defaults:
  concurrency: 8
  rate_limit: 50MB/s                # per repo, 0 = unlimited
  retention:
    grace_period: 72h

repos:
  - name: ubuntu
    type: apt
    upstream: https://archive.ubuntu.com/ubuntu
    prefix: ubuntu                  # key prefix in the bucket
    schedule: "0 */6 * * *"
    gpg_keyring: /etc/courier/keys/ubuntu-archive.gpg
    apt:
      suites: [noble, noble-updates, noble-security]
      components: [main, universe]
      architectures: [amd64]

  - name: fedora
    type: rpm
    upstream: https://dl.fedoraproject.org/pub/fedora/linux
    prefix: fedora
    schedule: "30 */6 * * *"
    rpm:
      releases: ["42"]
      repos: [releases, updates]
      architectures: [x86_64]

  - name: alpine
    type: apk
    upstream: https://dl-cdn.alpinelinux.org/alpine
    prefix: alpine
    schedule: "0 */3 * * *"
    apk:
      branches: [v3.20, v3.21]
      repos: [main, community]
      architectures: [x86_64]

server:
  listen: ":8080"
log:
  level: info
  format: json
```

Adding a release or architecture = editing the lists and reloading (ConfigMap change triggers a rollout via a checksum annotation in the chart).

## 6. Observability

### Metrics (`/metrics`, Prometheus)

| Metric | Type | Labels |
|---|---|---|
| `courier_sync_runs_total` | counter | repo, result |
| `courier_sync_duration_seconds` | histogram | repo |
| `courier_sync_last_success_timestamp_seconds` | gauge | repo |
| `courier_sync_in_progress` | gauge | repo |
| `courier_files` | gauge | repo, state (`mirrored`,`pending`,`failed`) |
| `courier_bytes_total` | counter | repo, direction (`downloaded`,`uploaded`) |
| `courier_errors_total` | counter | repo, stage, reason |
| `courier_upstream_request_duration_seconds` | histogram | repo, status |
| `courier_s3_request_duration_seconds` | histogram | op, status |
| `courier_gc_deleted_objects_total` | counter | repo |

### Logs

Structured JSON via `log/slog` to stdout. Every line carries `repo` and `run_id`; stage transitions (`plan`, `download`, `publish`, `gc`) are logged with counts and durations, so one failed sync can be followed end to end by `run_id`. Per-file failures include path, URL, and error.

### Probes and API

- `/healthz`: process alive. `/readyz`: config loaded and S3 reachable.
- `GET /status`: per-repo last run, result, duration, file counts.
- `POST /sync/{repo}`: trigger a run now (protected; see Security).

### Kubernetes extras shipped in the chart

- `ServiceMonitor` for the Prometheus Operator (toggle in values).
- `PrometheusRule`: `CourierSyncStale` (no success in 2x schedule interval), `CourierSyncFailing` (repeated failures), `CourierS3Errors`.
- Grafana dashboard as a ConfigMap (sidecar label), showing last sync age, duration, throughput, errors, file counts per repo.

## 7. Kubernetes deployment

- **Image:** multi-stage build, static Go binary, distroless/non-root, read-only root filesystem, pinned tags.
- **Helm chart (`charts/courier`):** Deployment (1 replica, `Recreate` strategy), ConfigMap (config), Secret reference (S3 credentials, or existing secret), Service (metrics/API), optional ServiceMonitor/PrometheusRule/dashboard, resource requests/limits, probes, `securityContext`, `terminationGracePeriodSeconds` long enough to finish or abort a sync cleanly.
- **Graceful shutdown:** on SIGTERM, stop scheduling, cancel in-flight work via context. Because publish is ordered and resumable, an interrupted run is safe to restart.
- **Scratch space:** downloads stream to S3 where possible; a small `emptyDir` for temporary files (size-limited).
- **External MinIO:** reached by DNS name from config; TLS CA mounted if private. A dedicated MinIO user with a policy limited to the mirror bucket.

## 8. Security

- S3 credentials only via Secret/env; never in the ConfigMap or logs.
- MinIO bucket policy: write for the `courier` user only; anonymous read on the mirror prefix only if you want LAN clients to use it without credentials.
- Upstream metadata signatures are verified before use; checksums are verified for every file.
- API write endpoints (`POST /sync`) not exposed outside the cluster by default; optional bearer token.
- Container runs as non-root with dropped capabilities.
- SSRF/path safety: upstream URLs come from config only; object keys are built from validated, sanitized relative paths (reject `..`).

## 9. Future: proxy-cache and OCI

- **Language ecosystems (Go modules, npm, PyPI):** the `proxy-cache` mode adds an HTTP front that serves from S3 and fetches from upstream on a miss. This needs a data-path component, unlike `sync`. It will be a separate `courier proxy` command/Deployment sharing `storage`, `fetcher`, and `observability`.
- **Container images (OCI):** pull-through cache for Docker Hub, ghcr.io, quay.io, etc.
- **Open decision, build vs wrap:** mature projects already exist for these (CNCF `distribution` registry has a pull-through mode with an S3 backend; Athens for Go modules). The default plan is to **deploy these through the same Helm chart as optional components, pointed at the same MinIO**, with the same Prometheus/Grafana setup, instead of reimplementing them. We revisit this when we reach that phase.

## 10. Testing strategy

- Unit tests per provider using recorded fixture metadata (small Release/repomd/APKINDEX samples).
- Engine tests against an in-memory `Storage` fake and an `httptest` upstream, including partial-sync resume, interrupted publish, and GC grace.
- Integration test with a local MinIO container (`make itest`).
- `golangci-lint`, `gosec`, `govulncheck` in `make check` (already configured).

## 11. Delivery plan

1. **Scaffold:** `git init`, `go mod`, config loading/validation, logging, metrics, health endpoints, Dockerfile, minimal Helm chart.
2. **Engine + storage + fetcher** with fakes and tests.
3. **APK provider** (smallest, simplest format) end to end against real MinIO.
4. **Chart hardening:** ServiceMonitor, PrometheusRule, Grafana dashboard.
5. **APT provider** (Ubuntu).
6. **RPM provider** (Fedora).
7. **Proxy-cache / OCI** (see section 9).

## 12. Decisions on earlier open questions

- **Upstreams:** official upstreams by default (configurable per repo).
- **Edge/testing:** not mirrored by default (`edge`, `updates-testing` are off). Can be enabled per repo in the config.
- **Access:** LAN only. The mirror is not exposed to the internet, so plain HTTP from MinIO is acceptable; TLS via a reverse proxy is optional. The anonymous read-only policy on the mirror prefix is enough, with no auth layer for clients.

## 13. Implementation notes (as built)

- **Alpine verification (optional).** Package and index signatures are mirrored untouched either way. If `apk.keys_dir` is set, the `APKINDEX.tar.gz` RSA signature (SHA-1 `.SIGN.RSA.` and SHA-256 `.SIGN.RSA256.`) is verified against those keys before anything is mirrored; if unset, Courier warns and mirrors the index as is (clients still verify it). Size and control-segment checks always run. Every `.apk` is checked for the size in the index and for the control-segment checksum (`C:` field, `Q1` = SHA-1 of the compressed control member). Verified against the real Alpine 3.20 index and key. Data-segment integrity is covered by the client, which verifies the package signature on install; Courier does not hash the data segment.
- **What "owned" means.** Courier only ever deletes objects listed in its own manifest (`_courier/state/<repo>.json`). It does not list the bucket, so unrelated objects under the same prefix are never touched.
- **Failure behaviour.** If any package fails, no metadata is published and the manifest is not saved. Already-uploaded packages are adopted on the next run (existence and size check) instead of being downloaded again.
- **Presence is checked in the bucket, never in the manifest.** Every run does a `Stat` per package: present with the right size means skip, anything else is downloaded again. After each upload the object is `Stat`ed again, so a store that acknowledges a PUT but does not keep the object fails the run before any metadata is published. The manifest is only used to decide which objects Courier owns (for garbage collection). Corruption of object contents (as opposed to missing or wrong-sized objects) is not detected; a periodic content re-verify is future work.
- **Scheduling.** One in-process cron per repo; a per-repo lock prevents overlap between scheduled runs, `POST /sync/{repo}` and `courier sync`. Running two pods against the same bucket is unsupported.
- **Unsupported types.** `apt` and `rpm` repos are accepted by the config but listed as `unsupported` and not scheduled until their providers exist.
- **Transient files.** Each package is downloaded to a temporary file, verified, then uploaded, so a bad file is never written to the bucket. Size the pod's `/tmp` for `concurrency` times the largest package.

### MinIO status (important for the S3 backend)

The open-source MinIO server and `mc` are archived upstream: no further releases, security updates, or downloadable binaries. Courier only uses the standard S3 API (path-style, Put/Get/Stat/Delete/BucketExists), so it should work with any S3-compatible store (for example Garage, SeaweedFS, Ceph RGW, or a commercial MinIO build). Consider the long-term plan for the store that holds the mirror. The S3 layer is tested against an in-process fake S3 server, not against a real MinIO.
