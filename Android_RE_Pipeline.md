# Android RE Pipeline — Implementation Plan

## Project Overview

An AI-assisted Android reverse engineering pipeline for security research and dead app revival.
Consists of two Go microservices, ephemeral Kubernetes Jobs for tool execution, NATS JetStream
for messaging, PostgreSQL for persistence, and a Claude skill for interaction.

---

## Architecture Summary

```
User / Claude Skill
        │
        ▼
Ingestion Service (Go)          ← POST /upload or POST /download
        │                          writes APK to NFS PVC
        │                          publishes message to NATS
        ▼
NATS JetStream (apk.ingested)
        │
        ▼
Coordinator Service (Go)        ← consumes NATS messages
        │                          creates k8s Jobs
        │                          tracks status in Postgres
        ▼
k8s Jobs (ephemeral)
  ├── jadx-wrapper
  ├── apktool-wrapper
  └── mobsf-submit
        │
        ▼
MobSF (long-running deployment) ← REST API, Postgres backend
        │
        ▼
NFS PVC (TrueNAS)               ← APK storage + decompiled output
```

---

## Repository Structure

```
android-re/
├── services/
│   ├── ingestion/              # Go service
│   │   ├── cmd/server/main.go
│   │   ├── internal/
│   │   │   ├── api/            # HTTP handlers
│   │   │   ├── sources/        # APKSource interface + implementations
│   │   │   │   ├── source.go   # interface definition
│   │   │   │   ├── directurl.go
│   │   │   │   └── apkpure.go
│   │   │   └── queue/          # NATS publisher
│   │   ├── Dockerfile
│   │   └── go.mod
│   │
│   └── coordinator/            # Go service
│       ├── cmd/server/main.go
│       ├── internal/
│       │   ├── api/            # HTTP handlers
│       │   ├── jobs/           # k8s Job creation + watching
│       │   ├── queue/          # NATS consumer
│       │   └── store/          # Postgres queries
│       ├── migrations/         # Goose SQL migration files
│       │   ├── 001_create_jobs.sql
│       │   └── 002_create_apk_metadata.sql
│       ├── Dockerfile
│       └── go.mod
│
├── images/                     # Tool wrapper images
│   ├── jadx/
│   │   └── Dockerfile
│   └── apktool/
│       └── Dockerfile
│
├── deploy/                     # Flux GitOps manifests
│   └── re-tools/
│       ├── namespace.yaml
│       ├── rbac.yaml
│       ├── pvc.yaml
│       ├── nats.yaml
│       ├── postgres-coordinator.yaml
│       ├── postgres-mobsf.yaml
│       ├── ingestion-deployment.yaml
│       ├── coordinator-deployment.yaml
│       ├── mobsf-deployment.yaml
│       └── sealed-secrets/
│           ├── postgres-coordinator-secret.yaml
│           ├── postgres-mobsf-secret.yaml
│           └── ghcr-pull-secret.yaml
│
└── skill/
    └── SKILL.md
```

---

## Phase 1: Infrastructure Manifests

### 1.1 Namespace and RBAC

File: `deploy/re-tools/namespace.yaml`
- Create namespace `re-tools`

File: `deploy/re-tools/rbac.yaml`
- ServiceAccount: `coordinator` in `re-tools` namespace
- Role: allow `create`, `get`, `list`, `delete` on `jobs` and `pods` resources
- RoleBinding: bind Role to coordinator ServiceAccount

### 1.2 PVC

File: `deploy/re-tools/pvc.yaml`
- Name: `re-tools-data`
- StorageClass: NFS (TrueNAS) — match existing storageClassName in cluster
- Access mode: `ReadWriteMany` (multiple Jobs need concurrent access)
- Size: start at 50Gi, adjust as needed
- Directory structure on volume:
  ```
  /data/
    apks/           ← uploaded/downloaded APKs
    output/         ← decompiled output per job_id
    mobsf/          ← MobSF uploads directory
  ```

### 1.3 NATS with JetStream

File: `deploy/re-tools/nats.yaml`
- Use official `nats:latest` image
- Single replica for homelab (no clustering needed)
- Args: `--jetstream --store_dir=/data`
- PVC: use `re-tools-data` or a dedicated small PVC for NATS persistence
- Service: ClusterIP on port 4222
- Resource limits: ~256Mi memory, 250m CPU
- ConfigMap for nats.conf if needed

### 1.4 PostgreSQL Clusters (CloudNativePG)

File: `deploy/re-tools/postgres-coordinator.yaml`
- Cluster name: `postgres-coordinator`
- 1 instance (homelab)
- Database: `coordinator`
- Secret ref: `postgres-coordinator-secret` (Sealed Secret)

File: `deploy/re-tools/postgres-mobsf.yaml`
- Cluster name: `postgres-mobsf`
- 1 instance
- Database: `mobsf`
- Secret ref: `postgres-mobsf-secret` (Sealed Secret)

### 1.5 Sealed Secrets

Create and seal secrets for:
- `postgres-coordinator-secret`: POSTGRES_USER, POSTGRES_PASSWORD, DATABASE_URL
- `postgres-mobsf-secret`: POSTGRES_USER, POSTGRES_PASSWORD, DATABASE_URL
- `ghcr-pull-secret`: Docker config JSON for ghcr.io pull access

---

## Phase 2: Tool Images

### 2.1 jadx Wrapper

File: `images/jadx/Dockerfile`
- Base: `eclipse-temurin:21-jre-alpine`
- Download jadx release JAR from GitHub releases
- Entrypoint: shell script that accepts `--apk <path> --output <path>`
- Writes decompiled Java source to output directory
- Exit 0 on success, non-zero on failure

### 2.2 apktool Wrapper

File: `images/apktool/Dockerfile`
- Base: `eclipse-temurin:21-jre-alpine`
- Install apktool
- Entrypoint: shell script that accepts `--apk <path> --output <path>`
- Writes smali + resources to output directory

### 2.3 GitHub Actions

File: `.github/workflows/build-images.yml`
- Trigger: push to `main` affecting `images/**`
- Matrix build for `jadx` and `apktool`
- Authenticate to ghcr.io using `GITHUB_TOKEN`
- Tag: `ghcr.io/<org>/re-<tool>:<git-sha>`
- Manual version bump in deployment manifests after push

---

## Phase 3: Ingestion Service

### 3.1 APKSource Interface

File: `services/ingestion/internal/sources/source.go`

```go
type APKMetadata struct {
    PackageName string
    Version     string
    VersionCode int
    Source      string
    DownloadURL string
    SHA256      string
}

type APKSource interface {
    Name() string
    Download(ctx context.Context, packageName, version string) (io.ReadCloser, *APKMetadata, error)
}
```

### 3.2 DirectURL Source

File: `services/ingestion/internal/sources/directurl.go`
- Implements `APKSource`
- Accepts a direct URL, HTTP GETs it, returns the reader
- Extracts package name from APK manifest using `aapt` or by parsing the ZIP
- Suitable for: direct links, Archive.org URLs

### 3.3 APKPure Source

File: `services/ingestion/internal/sources/apkpure.go`
- Implements `APKSource`
- Accepts package name + optional version
- Scrapes APKPure download page to resolve download URL
- Falls back to latest version if no version specified
- Note: respect rate limiting, add jitter between requests

### 3.4 HTTP API

File: `services/ingestion/internal/api/handlers.go`

Endpoints:
- `POST /upload` — multipart form upload of APK file
  - Writes to `/data/apks/<job_id>.apk`
  - Publishes to NATS
  - Returns `{ "job_id": "..." }`

- `POST /download` — JSON body `{ "source": "apkpure|directurl", "package_name": "...", "version": "...", "url": "..." }`
  - Kicks off background goroutine to download
  - Returns `{ "job_id": "..." }` immediately
  - Background goroutine: downloads APK → writes to PVC → publishes to NATS
  - On failure: publishes failure message to NATS

- `GET /health` — liveness probe

### 3.5 NATS Publisher

File: `services/ingestion/internal/queue/publisher.go`

Message schema published to subject `apk.ingested`:
```json
{
  "job_id": "uuid",
  "apk_path": "/data/apks/<job_id>.apk",
  "package_name": "com.example.app",
  "version": "1.2.3",
  "source": "apkpure",
  "submitted_at": "RFC3339"
}
```

On download failure, publish to `apk.ingestion.failed`:
```json
{
  "job_id": "uuid",
  "error": "..."
}
```

### 3.6 Deployment

File: `deploy/re-tools/ingestion-deployment.yaml`
- 1 replica
- Mount `re-tools-data` PVC at `/data`
- Env: NATS_URL, SERVICE_PORT
- Image: `ghcr.io/<org>/re-ingestion:<tag>`
- Service: ClusterIP (coordinator and skill only need internal access)
- Resource limits: 256Mi memory, 250m CPU

---

## Phase 4: Coordinator Service

### 4.1 Database Migrations (Goose)

File: `services/coordinator/migrations/001_create_jobs.sql`

```sql
-- +goose Up
CREATE TABLE jobs (
    id UUID PRIMARY KEY,
    status TEXT NOT NULL DEFAULT 'pending',  -- pending, running, complete, failed
    apk_path TEXT NOT NULL,
    package_name TEXT,
    version TEXT,
    source TEXT,
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    error TEXT,
    results_path TEXT
);

-- +goose Down
DROP TABLE jobs;
```

File: `services/coordinator/migrations/002_create_apk_metadata.sql`

```sql
-- +goose Up
CREATE TABLE apk_metadata (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id UUID REFERENCES jobs(id),
    package_name TEXT NOT NULL,
    version TEXT,
    version_code INT,
    sha256 TEXT,
    cert_sha256 TEXT,
    min_sdk INT,
    target_sdk INT,
    permissions TEXT[],
    ingested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE apk_metadata;
```

Run migrations on startup before serving:
```go
goose.SetDialect("postgres")
goose.Up(db, "migrations")
```

### 4.2 NATS Consumer

File: `services/coordinator/internal/queue/consumer.go`
- Connect to NATS JetStream on startup
- Create stream `APK_EVENTS` on subjects `apk.*` if not exists (idempotent)
- Create durable consumer named `coordinator` on `apk.ingested`
- On message receipt:
  1. Insert job record into Postgres (status: pending)
  2. Create k8s Jobs for jadx and apktool
  3. Update job status to `running`
  4. Ack message
- On processing error: Nak message for redelivery

### 4.3 Kubernetes Job Manager

File: `services/coordinator/internal/jobs/manager.go`

Uses `client-go` with in-cluster config (ServiceAccount):

```go
config, _ := rest.InClusterConfig()
clientset, _ := kubernetes.NewForConfig(config)
```

Job creation:
- Job name: `re-jadx-<job_id>` and `re-apktool-<job_id>`
- Namespace: `re-tools`
- Image: `ghcr.io/<org>/re-jadx:<tag>` (tag from coordinator env var)
- Args: `["--apk", "/data/apks/<job_id>.apk", "--output", "/data/output/<job_id>/jadx"]`
- Volume: mount `re-tools-data` PVC
- SecurityContext:
  ```yaml
  runAsNonRoot: true
  runAsUser: 1000
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
  ```
- NetworkPolicy: no egress (add separately)
- TTLSecondsAfterFinished: 300
- Resource limits: 2Gi memory, 1 CPU (jadx can be hungry)

Job watching using informers:
- Watch for Job completion/failure
- On complete: update job status in Postgres, set results_path
- On failed: update job status, capture pod logs as error message

### 4.4 HTTP API

File: `services/coordinator/internal/api/handlers.go`

Endpoints:
- `GET /status/{job_id}` — returns job status from Postgres
  ```json
  {
    "job_id": "...",
    "status": "running|complete|failed",
    "package_name": "com.example.app",
    "submitted_at": "...",
    "completed_at": "..."
  }
  ```

- `GET /results/{job_id}` — returns structured results once complete
  ```json
  {
    "job_id": "...",
    "results_path": "/data/output/<job_id>",
    "jadx": { "status": "complete", "source_path": "..." },
    "apktool": { "status": "complete", "manifest_path": "...", "smali_path": "..." },
    "mobsf": { "status": "complete", "report": { ... } }
  }
  ```

- `GET /results/{job_id}/file?path=<relative_path>` — returns contents of a specific
  file from the decompiled output (for skill to read source files)
  - Enforce path is within `/data/output/<job_id>/` to prevent traversal
  - Return raw text for .java/.smali/.xml files
  - Truncate at configurable limit (default 100KB) with truncation notice

- `GET /results/{job_id}/search?q=<query>` — grep across decompiled output
  - Returns matching file paths + line context
  - Useful for: finding hardcoded strings, API endpoints, class names

- `GET /health`

### 4.5 Deployment

File: `deploy/re-tools/coordinator-deployment.yaml`
- ServiceAccount: `coordinator`
- Mount `re-tools-data` PVC at `/data`
- Env: NATS_URL, DATABASE_URL, JADX_IMAGE_TAG, APKTOOL_IMAGE_TAG, SERVICE_PORT
- Image: `ghcr.io/<org>/re-coordinator:<tag>`
- Resource limits: 256Mi memory, 250m CPU

---

## Phase 5: MobSF Deployment

File: `deploy/re-tools/mobsf-deployment.yaml`
- Image: `opensecurity/mobile-security-framework-mobsf:latest`
- Env:
  - `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_HOST`, `POSTGRES_DB` from Sealed Secret
  - `MOBSF_API_KEY` from Sealed Secret
- Mount `re-tools-data` PVC at `/home/mobsf/.MobSF`
- Service: ClusterIP on port 8000
- Resource limits: 2Gi memory, 1 CPU
- Liveness probe: GET /api/v1/version with API key header

MobSF integration in coordinator:
- After jadx/apktool Jobs complete, submit APK to MobSF REST API
- Poll MobSF for scan completion
- Fetch and store JSON report in Postgres (apk_metadata table or separate report table)

---

## Phase 6: Network Policy

File: `deploy/re-tools/network-policy.yaml`

Job pods (matched by label `re-tools/job-type: analysis`):
- Deny all egress (untrusted APK content should not make network calls during analysis)
- Allow ingress from coordinator only

Coordinator:
- Allow egress to NATS (4222), Postgres (5432), MobSF (8000)
- Allow egress to k8s API server

Ingestion service:
- Allow egress to internet (for APK downloads)
- Allow egress to NATS (4222)

---

## Phase 7: GitHub Actions

File: `.github/workflows/build-ingestion.yml`
- Trigger: push to `main` affecting `services/ingestion/**`
- Build and push `ghcr.io/<org>/re-ingestion:<sha>`

File: `.github/workflows/build-coordinator.yml`
- Trigger: push to `main` affecting `services/coordinator/**`
- Build and push `ghcr.io/<org>/re-coordinator:<sha>`

File: `.github/workflows/build-images.yml`
- Trigger: push to `main` affecting `images/**`
- Matrix: jadx, apktool
- Build and push respective images

All workflows use `GITHUB_TOKEN` for ghcr.io auth — no additional secrets needed.

---

## Phase 8: Claude Skill

File: `skill/SKILL.md`

The skill gives Claude context to:

**Submit an APK for analysis:**
```
POST http://ingestion.re-tools.svc.cluster.local/upload
POST http://ingestion.re-tools.svc.cluster.local/download
```

**Poll for status:**
```
GET http://coordinator.re-tools.svc.cluster.local/status/{job_id}
```
Poll every 10s, timeout after 10 minutes.

**Fetch results:**
```
GET http://coordinator.re-tools.svc.cluster.local/results/{job_id}
GET http://coordinator.re-tools.svc.cluster.local/results/{job_id}/file?path=...
GET http://coordinator.re-tools.svc.cluster.local/results/{job_id}/search?q=...
```

**Common workflows the skill should describe:**
- "Find all hardcoded URLs/API endpoints" → search for `http`, `https`, `api`
- "Find hardcoded secrets/keys" → search for `key`, `secret`, `token`, `password`
- "Summarize permissions" → read AndroidManifest.xml from apktool output
- "Reconstruct API client" → read network-related classes from jadx output
- "What changed between versions" → submit both, diff the jadx output

---

## Build Order

1. Namespace, RBAC, PVC manifests → apply to cluster
2. Seal and apply secrets
3. NATS deployment → verify JetStream is working
4. CloudNativePG clusters → verify databases are up
5. MobSF deployment → verify it connects to Postgres and API is reachable
6. Build and push jadx + apktool images → verify manually with a test APK
7. Coordinator service → verify migrations run, NATS consumer starts, Jobs are created
8. Ingestion service → verify upload and download endpoints, NATS publish
9. End-to-end test with a known APK
10. Skill → test with Claude

---

## Key Dependencies

| Component | Package |
|-----------|---------|
| k8s client | `k8s.io/client-go` |
| NATS client | `github.com/nats-io/nats.go` |
| Postgres driver | `github.com/jackc/pgx/v5` |
| Migrations | `github.com/pressly/goose/v3` |
| HTTP router | `github.com/go-chi/chi/v5` |
| UUID | `github.com/google/uuid` |

---

## Open Questions / Future Work

- **APKMirror support** — requires more sophisticated HTTP client to bypass scraping protections
- **Google Play support** — needs Google account token management, consider if worth the operational overhead
- **Version diffing** — jadx output + `diff` or a smarter AST-level diff for comparing APK versions
- **gVisor** — stronger sandboxing for Job pods if untrusted APK concern grows
- **Pull-through registry cache** — if Job cold-start time becomes noticeable
- **Archive.org source** — good for truly dead apps, straightforward HTTP, no ToS concerns
- **Result retention policy** — CronJob to clean up old output from PVC after N days