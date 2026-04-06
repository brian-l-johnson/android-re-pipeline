# Android RE Pipeline

An AI-assisted Android reverse engineering pipeline for security research and dead app revival. Submit an APK (by file upload or direct URL), and the pipeline decompiles it with jadx and apktool, runs a MobSF static analysis scan, and stores everything so a Claude skill can query the results.

This repository contains the Go microservices, tool wrapper Dockerfiles, and GitHub Actions workflows. Kubernetes manifests (Flux Kustomizations, deployments, sealed secrets) live in a separate GitOps repo (`~/dev/homelab-gitops/`).

---

## Architecture

```
User / Claude Skill
        |
        v
Ingestion Service (Go)          <- POST /upload or POST /download
        |                          writes APK to NFS PVC
        |                          publishes message to NATS
        v
NATS JetStream (apk.ingested)
        |
        v
Coordinator Service (Go)        <- consumes NATS messages
        |                          creates k8s Jobs
        |                          tracks status in Postgres
        v
k8s Jobs (ephemeral)
  +-- jadx-wrapper
  +-- apktool-wrapper
  +-- mobsf-submit
        |
        v
MobSF (long-running deployment) <- REST API, Postgres backend
        |
        v
NFS PVC (TrueNAS)               <- APK storage + decompiled output
```

Data flow in brief:
1. Ingestion receives an APK (upload or download), writes it to the shared NFS PVC under `/data/apks/<job_id>.apk`, and publishes an `apk.ingested` message to NATS JetStream.
2. Coordinator consumes the message, inserts a job record into Postgres, and creates ephemeral Kubernetes Jobs for jadx and apktool.
3. The tool Jobs write decompiled output to `/data/output/<job_id>/` on the same NFS PVC.
4. Coordinator watches for Job completion, updates job status, and submits the APK to MobSF for static analysis.
5. Results are queryable through the Coordinator HTTP API.

---

## Repository Structure

```
android-re-pipeline/
+-- services/
|   +-- ingestion/              # Go HTTP service: accepts APK submissions
|   |   +-- cmd/server/main.go
|   |   +-- internal/
|   |   |   +-- api/            # HTTP handlers (POST /upload, POST /download)
|   |   |   +-- sources/        # APKSource interface + DirectURL implementation
|   |   |   +-- queue/          # NATS JetStream publisher
|   |   +-- Dockerfile
|   |   +-- go.mod
|   |
|   +-- coordinator/            # Go service: orchestrates analysis jobs
|       +-- cmd/server/main.go
|       +-- internal/
|       |   +-- api/            # HTTP handlers (status, results, file, search)
|       |   +-- jobs/           # Kubernetes Job creation and watching
|       |   +-- metadata/       # APK manifest parsing
|       |   +-- mobsf/          # MobSF REST API client
|       |   +-- queue/          # NATS JetStream consumer
|       |   +-- store/          # Postgres queries
|       +-- migrations/
|       |   +-- 001_create_jobs.sql
|       |   +-- 002_create_apk_metadata.sql
|       +-- Dockerfile
|       +-- go.mod
|
+-- images/                     # Tool wrapper container images
|   +-- jadx/
|   |   +-- Dockerfile          # eclipse-temurin:21 + jadx 1.5.1
|   |   +-- entrypoint.sh       # --apk <path> --output <path>
|   +-- apktool/
|       +-- Dockerfile          # eclipse-temurin:21 + apktool 2.10.0
|       +-- entrypoint.sh       # --apk <path> --output <path>
|
+-- .github/workflows/
|   +-- build-ingestion.yml     # builds re-ingestion image on push
|   +-- build-coordinator.yml   # builds re-coordinator image on push
|   +-- build-images.yml        # matrix build for jadx + apktool images
|
+-- skill/
    +-- SKILL.md                # Claude skill definition
```

GitOps manifests are maintained separately in `~/dev/homelab-gitops/`:
- `infrastructure/database/` — CloudNativePG Database CRDs and sealed secrets for coordinator and mobsf
- `apps/re-tools/` — all Kubernetes resources for the `re-tools` namespace (deployments, services, ingresses, PVCs, RBAC, network policies, sealed secrets)
- `clusters/production/re-tools.yaml` — Flux Kustomization

---

## Prerequisites

The target cluster must have the following operators and storage classes available:

| Requirement | Notes |
|---|---|
| Flux CD | GitOps reconciliation |
| NATS JetStream | Deployed as part of this app, but the operator/binary must be available |
| CloudNativePG | Manages the coordinator and MobSF Postgres clusters |
| StorageClass `nfs-kubestore` | Used for the shared 50Gi data PVC (ReadWriteMany) |
| StorageClass `cephfs` | Used for the NATS JetStream PVC (ReadWriteOnce, frequent fsync) |
| Sealed Secrets controller | Decrypts sealed secrets at deploy time |
| cert-manager | Issues TLS certificates for the three ingress endpoints |

Local tooling for the deploy steps below:
- `kubectl` with access to the cluster
- `kubeseal` configured to reach the in-cluster sealed-secrets controller
- `kustomize`

---

## Deployment

Kubernetes manifests are not in this repository. They are managed in `~/dev/homelab-gitops/` and applied by Flux. The steps below produce the files that get committed to that repo.

### 1. Create databases

From `~/dev/homelab-gitops/`:

```bash
./infrastructure/database/create-database.sh coordinator
./infrastructure/database/create-database.sh mobsf
```

This creates sealed DB password secrets, CloudNativePG Database CRDs, appends managed roles to `shared-postgres-cluster.yaml`, and updates `infrastructure/database/kustomization.yaml`. Requires live cluster access for `kubeseal`.

### 2. Scaffold the base application

From `~/dev/homelab-gitops/`:

```bash
./scaffold-app.sh re-tools ingestion.apps.blj.wtf ghcr.io/brian-l-johnson/re-ingestion:latest 8080
```

This creates `apps/re-tools/` with a namespace, deployment, service, ingress, certificate, and kustomization, and creates `clusters/production/re-tools.yaml`. The scaffolded deployment becomes the ingestion service baseline.

After scaffolding:
- Rename `deployment.yaml` to `ingestion-deployment.yaml`, and similarly for service, certificate, and ingress files.
- Edit `clusters/production/re-tools.yaml` to add `dependsOn: [infrastructure, database]`.

### 3. Create sealed secrets

These require live cluster access and must be created interactively:

```bash
# Coordinator secret (DATABASE_URL, MOBSF_API_KEY)
kubectl create secret generic coordinator-secret -n re-tools \
  --from-literal=DATABASE_URL='...' \
  --from-literal=MOBSF_API_KEY='...' \
  --dry-run=client -o yaml | kubeseal -o yaml > apps/re-tools/coordinator-secret-sealed.yaml

# MobSF secret (POSTGRES_USER, POSTGRES_PASSWORD, POSTGRES_HOST, POSTGRES_PORT, POSTGRES_DB, MOBSF_API_KEY)
kubectl create secret generic mobsf-secret -n re-tools \
  --from-literal=POSTGRES_USER='...' \
  ... \
  --dry-run=client -o yaml | kubeseal -o yaml > apps/re-tools/mobsf-secret-sealed.yaml

# GHCR pull secret
kubectl create secret docker-registry ghcr-pull-secret -n re-tools \
  --docker-server=ghcr.io \
  --docker-username=<github-user> \
  --docker-password=<pat> \
  --dry-run=client -o yaml | kubeseal -o yaml > apps/re-tools/ghcr-pull-secret-sealed.yaml
```

### 4. Add remaining manifests and push

The following resources are not produced by the scaffold script and must be created manually in `apps/re-tools/`:

- `rbac.yaml` — ServiceAccount `coordinator`, Role, RoleBinding
- `data-pvc.yaml` — 50Gi ReadWriteMany on `nfs-kubestore`
- `nats-pvc.yaml` — 2Gi ReadWriteOnce on `cephfs`
- `nats-deployment.yaml` and `nats-service.yaml` — NATS 2.10.27 with JetStream
- `coordinator-deployment.yaml`, `coordinator-service.yaml`, `coordinator-certificate.yaml`, `coordinator-ingress.yaml`
- `mobsf-deployment.yaml`, `mobsf-service.yaml`, `mobsf-certificate.yaml`, `mobsf-ingress.yaml`
- `network-policy.yaml` — egress controls (analysis Jobs get all-egress-denied; see PLAN.md Phase 2 Task 2.7)

Update `apps/re-tools/kustomization.yaml` to include all resources, then verify:

```bash
kustomize build apps/re-tools/
```

Commit and push to the GitOps repo. Flux will reconcile.

### 5. Build and push images

Images are built automatically by GitHub Actions on push to `main`:
- `services/ingestion/**` triggers `build-ingestion.yml` → `ghcr.io/brian-l-johnson/re-ingestion:<sha>`
- `services/coordinator/**` triggers `build-coordinator.yml` → `ghcr.io/brian-l-johnson/re-coordinator:<sha>`
- `images/**` triggers `build-images.yml` (matrix) → `re-jadx:<sha>` and `re-apktool:<sha>`

All workflows authenticate to ghcr.io using `GITHUB_TOKEN`. No additional secrets are needed in GitHub.

After pushing images, update the image tags in the GitOps manifests if you are pinning to a specific SHA rather than `latest`.

---

## Usage

### Ingest endpoints (Ingestion service)

**Upload an APK file:**

```
POST https://ingestion.apps.blj.wtf/upload
Content-Type: multipart/form-data

file=@/path/to/app.apk
```

**Download an APK from a direct URL:**

```
POST https://ingestion.apps.blj.wtf/download
Content-Type: application/json

{
  "source": "directurl",
  "identifier": "https://example.com/app.apk"
}
```

Both endpoints return:

```json
{ "job_id": "550e8400-e29b-41d4-a716-446655440000" }
```

The `POST /download` endpoint is synchronous and blocks until the download completes (up to 10 minutes). Return status 201 on success, 4xx/5xx with `{"error": "..."}` on failure.

### Poll for status (Coordinator service)

```
GET https://coordinator.apps.blj.wtf/status/{job_id}
```

Response:

```json
{
  "job_id": "...",
  "status": "pending|running|complete|failed",
  "package_name": "com.example.app",
  "submitted_at": "2026-04-05T10:00:00Z",
  "completed_at": "2026-04-05T10:03:12Z"
}
```

Poll every 10 seconds. Typical analysis completes in 2-5 minutes depending on APK size.

### Fetch results

**Summary:**
```
GET https://coordinator.apps.blj.wtf/results/{job_id}
```

Returns paths and status for each tool (jadx, apktool, mobsf) plus the MobSF JSON report.

**Read a specific file from decompiled output:**
```
GET https://coordinator.apps.blj.wtf/results/{job_id}/file?path=jadx/com/example/MainActivity.java
```

Paths are relative to `/data/output/<job_id>/`. Responses are capped at 100KB with a truncation notice. Path traversal outside the job's output directory is rejected.

**Search decompiled output:**
```
GET https://coordinator.apps.blj.wtf/results/{job_id}/search?q=api_key
```

Returns matching file paths with line context. Useful for finding hardcoded strings, API endpoints, class names, and secrets.

### Ingress endpoints

| Hostname | Service | Purpose |
|---|---|---|
| `ingestion.apps.blj.wtf` | Ingestion service (port 8080) | APK submission (upload + download) |
| `coordinator.apps.blj.wtf` | Coordinator service (port 8080) | Job status, results, file access, search |
| `mobsf.apps.blj.wtf` | MobSF (port 8000) | MobSF web UI and REST API (direct access) |

All three use cert-manager TLS certificates issued by `letsencrypt-prod`.

Within the cluster, the ingestion and coordinator services are also reachable via ClusterIP:
- `http://ingestion.re-tools.svc.cluster.local/`
- `http://coordinator.re-tools.svc.cluster.local/`
- `http://mobsf.re-tools.svc.cluster.local:8000/`

---

## Building Locally

```bash
# Services
cd services/ingestion  && go build ./cmd/server
cd services/coordinator && go build ./cmd/server

# Tests
cd services/ingestion  && go test ./...
cd services/coordinator && go test ./...

# Lint (requires golangci-lint)
cd services/ingestion  && golangci-lint run
cd services/coordinator && golangci-lint run

# Docker (build from repo root for services)
docker build -f services/ingestion/Dockerfile  -t re-ingestion:dev  .
docker build -f services/coordinator/Dockerfile -t re-coordinator:dev .

# Docker (tool images — build from image directory)
docker build -f images/jadx/Dockerfile    -t re-jadx:dev    images/jadx/
docker build -f images/apktool/Dockerfile -t re-apktool:dev images/apktool/
```

---

## Parking Lot / Future Work

Items deferred from the current implementation:

- **APKPure source** — scraping-based, needs rate limiting and jitter. The `APKSource` interface is designed to accommodate it without changes to the rest of the pipeline.
- **APKMirror source** — requires a more sophisticated HTTP client to handle scraping protections.
- **Google Play source** — needs Google account token management; operational overhead may not be worth it.
- **Archive.org source** — good for truly dead apps, straightforward HTTP, no ToS concerns.
- **Version diffing** — submit two versions, diff jadx output. Could be AST-level rather than line-level for more meaningful diffs.
- **gVisor sandboxing** — stronger isolation for analysis Job pods when running untrusted APK content.
- **Pull-through registry cache** — reduce cold-start time for Job pods if image pull latency becomes noticeable.
- **Result retention policy** — CronJob to clean up `/data/output/` after N days to keep NFS usage bounded.
