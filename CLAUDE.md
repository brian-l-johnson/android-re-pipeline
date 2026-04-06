# Android RE Pipeline — Developer Guide

## Environment

Go is installed via MacPorts. The binary is at `/opt/local/bin/go` and is not on the default tool PATH. Always use the full path or prepend to PATH when running Go commands:

```bash
export PATH="/opt/local/bin:$PATH"
# or use directly:
/opt/local/bin/go build ./...
```

`swag` and other Go tools installed via `go install` live at `~/go/bin/`.

## Repository Overview

This is a multi-module Go repository containing two services under `services/`:

- `services/ingestion` — HTTP service that accepts APK submission requests and publishes jobs to NATS
- `services/coordinator` — Consumes NATS jobs, orchestrates analysis (jadx, apktool, MobSF), stores results in Postgres

Each service has its own `go.mod`. There is no top-level Go module.

GitOps manifests (Flux, Kubernetes YAML) live in a **separate repo**: `~/dev/homelab-gitops/`.

---

## Build

```bash
cd services/ingestion && go build ./cmd/server
cd services/coordinator && go build ./cmd/server
```

## Test

```bash
cd services/ingestion && go test ./...
cd services/coordinator && go test ./...
```

## Lint

Run from within the service directory (requires `golangci-lint` in PATH):

```bash
cd services/ingestion && golangci-lint run
cd services/coordinator && golangci-lint run
```

## Docker — Services

Build from the **repo root** (context is the repo root so Dockerfiles can reference shared files if needed):

```bash
docker build -f services/ingestion/Dockerfile -t re-ingestion:dev .
docker build -f services/coordinator/Dockerfile -t re-coordinator:dev .
```

## Docker — Tool Wrapper Images

Build from the image-specific directory (each image is self-contained):

```bash
docker build -f images/jadx/Dockerfile    -t re-jadx:dev    images/jadx/
docker build -f images/apktool/Dockerfile -t re-apktool:dev images/apktool/
```

## Swagger / OpenAPI Docs

The coordinator service uses [swaggo](https://github.com/swaggo/swag) to generate OpenAPI docs from annotations in `internal/api/handlers.go`. The generated `docs/` package is embedded in the binary at compile time.

**After adding or modifying any HTTP handler or its `@Summary`/`@Param`/`@Success` annotations, always regenerate the docs:**

```bash
cd services/coordinator && ~/go/bin/swag init -g cmd/server/main.go --output docs
```

Commit the updated `docs/` files alongside the handler changes. CI also runs `swag init` before the Docker build as a safety net, but the committed docs should always be kept current so diffs are meaningful.

## GitOps

Kubernetes manifests and Flux Kustomizations are managed in a separate repo:

```
~/dev/homelab-gitops/
```

Do not commit Kubernetes YAML to this repo.
