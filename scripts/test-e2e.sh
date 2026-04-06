#!/usr/bin/env bash
# test-e2e.sh — end-to-end integration test for the Android RE pipeline.
#
# Requires:
#   - curl, jq installed
#   - A running cluster with ingestion + coordinator deployed
#   - Env vars (or defaults below):
#       INGESTION_URL    — e.g. https://ingestion.apps.blj.wtf
#       COORDINATOR_URL  — e.g. https://coordinator.apps.blj.wtf
#       APK_URL          — direct URL to a small open-source APK to download
#       POLL_INTERVAL    — seconds between status polls (default: 15)
#       POLL_TIMEOUT     — max seconds to wait for completion (default: 600)

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
INGESTION_URL="${INGESTION_URL:-https://ingestion.apps.blj.wtf}"
COORDINATOR_URL="${COORDINATOR_URL:-https://coordinator.apps.blj.wtf}"

# F-Droid client APK — small, well-known, open-source
APK_URL="${APK_URL:-https://f-droid.org/F-Droid.apk}"

POLL_INTERVAL="${POLL_INTERVAL:-15}"
POLL_TIMEOUT="${POLL_TIMEOUT:-600}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[$(date -u +%H:%M:%S)] $*"; }
fail() { echo "[FAIL] $*" >&2; exit 1; }
pass() { echo "[PASS] $*"; }

require_cmd() {
  command -v "$1" &>/dev/null || fail "Required command not found: $1"
}

require_cmd curl
require_cmd jq

# ---------------------------------------------------------------------------
# Step 1 — Health checks
# ---------------------------------------------------------------------------
log "Checking service health..."

INGESTION_HEALTH=$(curl -sf "${INGESTION_URL}/health" || fail "Ingestion health check failed")
COORDINATOR_HEALTH=$(curl -sf "${COORDINATOR_URL}/health" || fail "Coordinator health check failed")

echo "$INGESTION_HEALTH"   | jq -e '.status == "ok"' &>/dev/null || fail "Ingestion not healthy: $INGESTION_HEALTH"
echo "$COORDINATOR_HEALTH" | jq -e '.status == "ok"' &>/dev/null || fail "Coordinator not healthy: $COORDINATOR_HEALTH"

pass "Both services are healthy"

# ---------------------------------------------------------------------------
# Step 2 — Submit APK via /download (directurl source)
# ---------------------------------------------------------------------------
log "Submitting APK from: ${APK_URL}"

SUBMIT_RESP=$(curl -sf -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"source\":\"directurl\",\"identifier\":\"${APK_URL}\"}" \
  "${INGESTION_URL}/download") || fail "Failed to submit APK"

log "Submit response: $SUBMIT_RESP"
JOB_ID=$(echo "$SUBMIT_RESP" | jq -re '.job_id') || fail "No job_id in submit response: $SUBMIT_RESP"
pass "APK submitted, job_id=${JOB_ID}"

# ---------------------------------------------------------------------------
# Step 3 — Poll coordinator /status until complete or failed
# ---------------------------------------------------------------------------
log "Polling status for job ${JOB_ID} (timeout: ${POLL_TIMEOUT}s)..."

ELAPSED=0
STATUS=""
while true; do
  STATUS_RESP=$(curl -sf "${COORDINATOR_URL}/status/${JOB_ID}") \
    || fail "Status request failed for job ${JOB_ID}"
  STATUS=$(echo "$STATUS_RESP" | jq -re '.status')

  log "status=${STATUS} jadx=$(echo "$STATUS_RESP" | jq -re '.jadx_status') apktool=$(echo "$STATUS_RESP" | jq -re '.apktool_status') mobsf=$(echo "$STATUS_RESP" | jq -re '.mobsf_status')"

  if [[ "$STATUS" == "complete" ]]; then
    pass "Job complete"
    break
  fi
  if [[ "$STATUS" == "failed" ]]; then
    ERROR=$(echo "$STATUS_RESP" | jq -re '.error // "unknown"')
    fail "Job failed: ${ERROR}"
  fi

  if [[ $ELAPSED -ge $POLL_TIMEOUT ]]; then
    fail "Timed out waiting for job completion after ${POLL_TIMEOUT}s (current status: ${STATUS})"
  fi

  sleep "$POLL_INTERVAL"
  ELAPSED=$((ELAPSED + POLL_INTERVAL))
done

# ---------------------------------------------------------------------------
# Step 4 — Verify /results response structure
# ---------------------------------------------------------------------------
log "Fetching results for job ${JOB_ID}..."

RESULTS=$(curl -sf "${COORDINATOR_URL}/results/${JOB_ID}") \
  || fail "Failed to fetch results for job ${JOB_ID}"

# Verify top-level fields
echo "$RESULTS" | jq -e '.job_id == "'"${JOB_ID}"'"'     &>/dev/null || fail "results: missing or wrong job_id"
echo "$RESULTS" | jq -e '.status == "complete"'          &>/dev/null || fail "results: status is not complete"
echo "$RESULTS" | jq -e '.jadx.status == "complete"'     &>/dev/null || fail "results: jadx not complete"
echo "$RESULTS" | jq -e '.apktool.status == "complete"'  &>/dev/null || fail "results: apktool not complete"
echo "$RESULTS" | jq -e '.mobsf.status == "complete"'    &>/dev/null || fail "results: mobsf not complete"
echo "$RESULTS" | jq -e '.mobsf.report != null'          &>/dev/null || fail "results: mobsf report is null"

pass "Results structure verified"

# Verify metadata
echo "$RESULTS" | jq -e '.metadata.package_name != null and .metadata.package_name != ""' &>/dev/null \
  || fail "results: metadata.package_name is missing"
echo "$RESULTS" | jq -e '.metadata.min_sdk > 0'    &>/dev/null || fail "results: metadata.min_sdk is 0 or missing"
echo "$RESULTS" | jq -e '.metadata.target_sdk > 0' &>/dev/null || fail "results: metadata.target_sdk is 0 or missing"

pass "Metadata fields verified"

# ---------------------------------------------------------------------------
# Step 5 — Verify /tree returns file listings
# ---------------------------------------------------------------------------
log "Verifying tree endpoint..."

TREE_RESP=$(curl -sf "${COORDINATOR_URL}/results/${JOB_ID}/tree") \
  || fail "Tree request failed"
echo "$TREE_RESP" | jq -e '.entries | length > 0' &>/dev/null \
  || fail "tree: entries is empty or missing"
pass "Root tree listing returned entries"

# Check jadx subtree
JADX_TREE=$(curl -sf "${COORDINATOR_URL}/results/${JOB_ID}/tree?path=jadx") \
  || fail "Tree request for jadx failed"
echo "$JADX_TREE" | jq -e '.entries | length > 0' &>/dev/null \
  || fail "tree/jadx: entries is empty"
pass "jadx tree listing returned entries"

# Check apktool subtree
APKTOOL_TREE=$(curl -sf "${COORDINATOR_URL}/results/${JOB_ID}/tree?path=apktool") \
  || fail "Tree request for apktool failed"
echo "$APKTOOL_TREE" | jq -e '.entries | length > 0' &>/dev/null \
  || fail "tree/apktool: entries is empty"
pass "apktool tree listing returned entries"

# ---------------------------------------------------------------------------
# Step 6 — Verify /file can serve AndroidManifest.xml
# ---------------------------------------------------------------------------
log "Verifying file endpoint (AndroidManifest.xml)..."

MANIFEST=$(curl -sf "${COORDINATOR_URL}/results/${JOB_ID}/file?path=apktool/AndroidManifest.xml") \
  || fail "File request for AndroidManifest.xml failed"

# Basic sanity: manifest XML should contain "manifest" and "package"
echo "$MANIFEST" | grep -q "manifest" || fail "AndroidManifest.xml content looks wrong (no 'manifest' string)"
echo "$MANIFEST" | grep -q "package"  || fail "AndroidManifest.xml content looks wrong (no 'package' string)"
pass "AndroidManifest.xml file served successfully"

# ---------------------------------------------------------------------------
# Step 7 — Verify /search returns matches for a common string
# ---------------------------------------------------------------------------
log "Verifying search endpoint..."

SEARCH_RESP=$(curl -sf "${COORDINATOR_URL}/results/${JOB_ID}/search?q=android&max=10") \
  || fail "Search request failed"
echo "$SEARCH_RESP" | jq -e '.matches | length > 0' &>/dev/null \
  || fail "search: no matches found for 'android'"
echo "$SEARCH_RESP" | jq -e '.query == "android"' &>/dev/null \
  || fail "search: response missing query field"
pass "Search returned matches"

# ---------------------------------------------------------------------------
# Step 8 — Verify path traversal is rejected
# ---------------------------------------------------------------------------
log "Verifying path traversal protection..."

TRAVERSAL_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  "${COORDINATOR_URL}/results/${JOB_ID}/tree?path=../other-job")
[[ "$TRAVERSAL_CODE" == "400" ]] \
  || fail "Path traversal not rejected: got HTTP ${TRAVERSAL_CODE}, expected 400"
pass "Path traversal correctly rejected (HTTP 400)"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "======================================================"
echo "  All e2e tests passed for job ${JOB_ID}"
echo "======================================================"
