# Courier

Courier is a self-hosted package mirror for a homelab. It is meant to replace a
collection of per-distro tools (debmirror, `dnf reposync`, ...) with one Go
application that syncs repositories into an S3 bucket (MinIO) and runs on
Kubernetes.

Planned ecosystems:

- **Distros (scheduled, filtered full mirror):** Ubuntu (APT), Fedora (RPM), Alpine (APK)
- **Later (proxy-cache):** Go modules, npm, PyPI, container images

Mirrors are byte-for-byte copies of upstream, so upstream signatures stay valid and
Courier never needs a signing key. Clients download directly from MinIO; Courier only
manages the sync.

## Status

Early but functional for Alpine. What works today:

- **Alpine (APK) mirroring**: verifies the signed `APKINDEX.tar.gz` against your trusted keys,
  checks each package's size and control-segment checksum against the index, uploads to S3,
  publishes the index last, and garbage-collects removed packages after a grace period.
- Scheduler (cron per repo), `courier sync <repo>`, and `POST /sync/{repo}`.
- Config loading and validation, structured JSON logs, Prometheus metrics, health endpoints.
- Dockerfile and Helm chart (with optional ServiceMonitor and alert rules).

**Not implemented yet:** the RPM (Fedora) provider. Repos of that type are
listed in `/status` as `unsupported` and are not scheduled. See the delivery plan in
[docs/architecture.md](docs/architecture.md#11-delivery-plan).

## Quick start

Requirements: Go 1.27+.

```sh
go build -o bin/courier ./cmd/courier

# Check a config file
bin/courier validate -c config.example.yaml

# Run the server
bin/courier serve -c config.example.yaml
curl localhost:8080/healthz
curl localhost:8080/status
curl localhost:8080/metrics | grep courier_
```

### Commands

| Command | Description |
|---|---|
| `courier serve -c config.yaml` | Run the scheduler, API and metrics |
| `courier validate -c config.yaml` | Validate the config file and exit |
| `courier sync -c config.yaml <repo>` | Run one sync now in the foreground and exit |
| `courier version` | Print the version |

### HTTP API

| Endpoint | Description |
|---|---|
| `GET /healthz`, `GET /readyz` | Liveness; readiness (checks the S3 bucket) |
| `GET /status` | Per-repo last run, result, counts |
| `POST /sync/{repo}` | Start a sync now (`202`, `404` unknown repo, `409` already running) |
| `GET /metrics` | Prometheus metrics |

There is no authentication on the API; keep the Service `ClusterIP`/LAN-only.
## Configuration

One YAML file; unknown fields are rejected. See [config.example.yaml](config.example.yaml)
for a complete example covering Ubuntu, Fedora and Alpine.

- `storage`: MinIO endpoint, bucket, TLS and path-style options. Credentials are never
  in the file: `access_key_env` and `secret_key_env` name the environment variables
  that hold them.
- `defaults`: concurrency, rate limit, retention grace period.
- `repos`: one entry per mirrored repository, each with a `type` (`apt`, `rpm`, `apk`),
  `upstream`, bucket `prefix`, cron `schedule` and a type-specific section that selects
  releases, repos/components and architectures.
- `server`, `log`: listen address, log level (`debug|info|warn|error`) and format
  (`json|text`).

Package signatures are never altered: files are copied byte-for-byte, and your clients verify them with
their own keys. For Alpine you can optionally set `apk.keys_dir` (a directory of trusted `*.rsa.pub` keys from
https://alpinelinux.org/keys/) so that Courier also verifies the upstream index before mirroring it; if unset,
Courier logs a warning and mirrors the index unverified.

For Ubuntu/Debian, `gpg_keyring` is an optional OpenPGP keyring file (binary or armored), for example
`ubuntu-archive-keyring.gpg` from https://archive.ubuntu.com/ubuntu/project/. `apt.languages` (default `[en]`)
selects the `Translation-*` files and `apt.contents: true` adds the very large `Contents-<arch>` indexes. Source
packages, `dep11`, `cnf` and debian-installer indexes are not mirrored.

**Size warning:** a full Ubuntu release is hundreds of GB per architecture (`main` plus `universe`). Start with
a single suite, component and architecture.

Releases and architectures are plain lists, so adding one is a config change.

## Observability

Metrics are exposed at `/metrics`:

| Metric | Type |
|---|---|
| `courier_sync_runs_total{repo,result}` | counter |
| `courier_sync_duration_seconds{repo}` | histogram |
| `courier_sync_last_success_timestamp_seconds{repo}` | gauge |
| `courier_sync_in_progress{repo}` | gauge |
| `courier_files{repo,state}` | gauge |
| `courier_bytes_total{repo,direction}` | counter |
| `courier_errors_total{repo,stage,reason}` | counter |
| `courier_upstream_request_duration_seconds{repo,status}` | histogram |
| `courier_s3_request_duration_seconds{op,status}` | histogram |
| `courier_gc_deleted_objects_total{repo}` | counter |
| `courier_build_info{version}` | gauge |

Logs are JSON on stderr. Every sync log line is tagged with `repo` and `run_id`.
The Helm chart can create a `ServiceMonitor` and a `PrometheusRule` (`CourierSyncStale`,
`CourierSyncFailing`, `CourierDown`).

## Container image and Kubernetes

```sh
# Build (podman or docker)
podman build -f deploy/Dockerfile --build-arg VERSION=0.1.0 -t courier:0.1.0 .

# S3 credentials: keys must match the env var names in the config
kubectl create secret generic courier-s3 \
  --from-literal=S3_ACCESS_KEY=... --from-literal=S3_SECRET_KEY=...

# Install with your own values (config.repos must not be empty)
helm install courier charts/courier -f my-values.yaml \
  --set image.tag=0.1.0 \
  --set serviceMonitor.enabled=true \
  --set prometheusRule.enabled=true
```

The chart runs a single replica with the `Recreate` strategy (one writer per repo), a
non-root user, a read-only root filesystem and probes on `/healthz` and `/readyz`. The
config is rendered from `values.yaml` under the `config:` key into a ConfigMap, and the
pod restarts when it changes. MinIO is expected to run outside the cluster.

## Development

```sh
go build ./...
go vet ./...
go test -cover ./...
golangci-lint run
gosec ./...
govulncheck ./...
```

The `Makefile` wraps these (`make check`). `go test -race` needs a C compiler; the
Makefile skips `-race` if `gcc` is not installed. Lint rules live in
[.golangci.yml](.golangci.yml).

### Layout

```
cmd/courier/            CLI entry point
internal/config/        config loading and validation
internal/observability/ logging and Prometheus metrics
internal/server/        HTTP endpoints
internal/syncer/        sync engine, runner (scheduler + status)
internal/provider/      provider interface; apk/ and apt/ implementations
internal/storage/       S3 (minio-go) and in-memory stores
internal/fetcher/       HTTP downloads: retries, rate limit, stall detection
internal/state/         per-repo manifest stored in the bucket
charts/courier/         Helm chart
deploy/Dockerfile       container image
docs/architecture.md    design, decisions and delivery plan
```

## Design

The full design (sync flow, provider interface, publish ordering, security notes and
the proxy-cache plan) is in [docs/architecture.md](docs/architecture.md).
