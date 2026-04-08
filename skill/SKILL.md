# Android RE Pipeline — Claude Skill

Use this skill to submit APKs for analysis and browse the results using the
Android RE pipeline. All interactions are via HTTP — no cluster access required.

## Base URLs

| Service     | URL                                  |
|-------------|--------------------------------------|
| Ingestion   | `https://ingestion.apps.blj.wtf`     |
| Coordinator | `https://coordinator.apps.blj.wtf`   |

---

## Submitting an APK

### Upload a local file

```bash
curl -X POST \
  -F 'file=@/path/to/app.apk' \
  https://ingestion.apps.blj.wtf/upload
```

Response: `{"job_id": "<uuid>"}`

### Download from a direct URL

```bash
curl -X POST \
  -H 'Content-Type: application/json' \
  -d '{"source":"directurl","identifier":"https://example.com/app.apk"}' \
  https://ingestion.apps.blj.wtf/download
```

Response: `{"job_id": "<uuid>"}`

This call blocks until the APK is fully downloaded to the pipeline (may take
seconds to minutes for large APKs). The `source` field must be `"directurl"`.

---

## Checking Job Status

```bash
curl https://coordinator.apps.blj.wtf/status/<job_id>
```

Poll every 15 seconds until `status` is `"complete"` or `"failed"`.
Typical analysis takes 2–5 minutes.

Key fields in the response:

| Field           | Meaning                                             |
|-----------------|-----------------------------------------------------|
| `status`        | `pending`, `running`, `complete`, `failed`          |
| `jadx_status`   | `pending`, `running`, `complete`, `failed`          |
| `apktool_status`| `pending`, `running`, `complete`, `failed`          |
| `mobsf_status`  | `pending`, `running`, `complete`, `failed`          |
| `error`         | Error message if the job failed                     |

Shell polling loop:

```bash
JOB_ID="<uuid>"
while true; do
  STATUS=$(curl -s https://coordinator.apps.blj.wtf/status/$JOB_ID | jq -r '.status')
  echo "$(date -u +%H:%M:%S) status=$STATUS"
  [[ "$STATUS" == "complete" || "$STATUS" == "failed" ]] && break
  sleep 15
done
```

---

## Retrieving Results

Once a job is `complete`, fetch the full results summary:

```bash
curl https://coordinator.apps.blj.wtf/results/<job_id>
```

This returns:
- `metadata` — package name, version, SDK levels, permissions, activities,
  services, receivers (extracted from AndroidManifest.xml)
- `jadx` — status and output path for the Java decompilation
- `apktool` — status and output path for the smali + resource disassembly
- `mobsf` — status and the full MobSF JSON security report

---

## Downloading Results as an Archive

Download the complete output directory (jadx + apktool output) as a zip or tar.gz:

```bash
# zip (default)
curl -L -o results.zip \
  "https://coordinator.apps.blj.wtf/results/<job_id>/download?format=zip"

# tar.gz
curl -L -o results.tar.gz \
  "https://coordinator.apps.blj.wtf/results/<job_id>/download?format=tar.gz"
```

The filename returned in `Content-Disposition` will be `<package_name>-<version>.zip`
when package info is available, otherwise `<job_id>.zip`.
Large outputs (jadx can produce 100–500 MB) stream directly — no server-side
buffering — so the download starts immediately.

---

## Browsing the File Tree

List files at a path within the job's output directory:

```bash
# List the root of the job output
curl "https://coordinator.apps.blj.wtf/results/<job_id>/tree"

# List jadx decompiled Java sources
curl "https://coordinator.apps.blj.wtf/results/<job_id>/tree?path=jadx/sources"

# Drill into a package
curl "https://coordinator.apps.blj.wtf/results/<job_id>/tree?path=jadx/sources/com/example/app"

# List apktool smali output
curl "https://coordinator.apps.blj.wtf/results/<job_id>/tree?path=apktool/smali"
```

Response:
```json
{
  "path": "jadx/sources/com/example/app",
  "entries": [
    { "name": "MainActivity.java", "type": "file", "size": 12345 },
    { "name": "utils", "type": "dir" }
  ]
}
```

---

## Reading a File

```bash
curl "https://coordinator.apps.blj.wtf/results/<job_id>/file?path=jadx/sources/com/example/app/MainActivity.java"
```

- Returns raw text content (Content-Type: text/plain)
- Files larger than 100 KB are truncated; check the `X-Truncated: true` header
- Use the path from a tree listing — always relative to the job output root

Common files to read:

| File                                           | Contents                        |
|------------------------------------------------|---------------------------------|
| `apktool/AndroidManifest.xml`                  | Decoded manifest                |
| `jadx/sources/com/example/MainActivity.java`   | Main activity (decompiled Java) |
| `apktool/smali/com/example/MainActivity.smali` | Main activity (smali bytecode)  |

---

## Searching Within Results

```bash
# Find API endpoint strings
curl "https://coordinator.apps.blj.wtf/results/<job_id>/search?q=https%3A%2F%2Fapi"

# With a result limit
curl "https://coordinator.apps.blj.wtf/results/<job_id>/search?q=Bearer&max=100"
```

Searched file types: `.java`, `.smali`, `.xml`, `.json`
Default limit: 50 results, maximum: 200.
Search times out after 30 seconds.

Response:
```json
{
  "query": "Bearer",
  "matches": [
    {
      "file": "jadx/sources/com/example/api/ApiClient.java",
      "line": 42,
      "context": "headers.put(\"Authorization\", \"Bearer \" + token);"
    }
  ],
  "truncated": false
}
```

---

## Recommended Approach: Download First

For any meaningful analysis, **download the archive first** and work locally
rather than making repeated API calls to read individual files. The API file
and search endpoints are useful for quick one-off lookups, but local tools are
faster, have no result caps, and cover all file types.

```bash
# Download and extract into a working directory
JOB_ID="<uuid>"
curl -L -o /tmp/apk-results.zip \
  "https://coordinator.apps.blj.wtf/results/$JOB_ID/download?format=zip"
unzip -q /tmp/apk-results.zip -d /tmp/apk-$JOB_ID
cd /tmp/apk-$JOB_ID
```

Then use local tools for all subsequent analysis:

```bash
# Search across everything — no type restrictions, no result caps
grep -r "api.example.com" .
grep -rn "Bearer\|api_key\|secret\|password" . --include="*.java"

# Find all network-related classes
find . -name "*.java" | xargs grep -l "OkHttp\|Retrofit\|HttpURLConnection"

# Scan for hardcoded URLs
grep -roh 'https\?://[^"'\'']\+' . | sort -u

# Extract all strings from native libs
find apktool/lib -name "*.so" -exec strings {} \; | grep -i "api\|key\|token"

# Count obfuscation level (short class names indicate ProGuard)
find jadx/sources -name "*.java" | awk -F/ '{print $NF}' | \
  awk -F. '{print length($1)}' | sort -n | head -20
```

Only fall back to the API search/file endpoints when you need a quick check
without downloading, or when the archive is too large to fetch in full.

---

## Common Analysis Workflows

### Find API endpoints
```bash
# HTTP/HTTPS strings
curl ".../search?q=https%3A%2F%2F&max=100"
# Retrofit or OkHttp base URLs
curl ".../search?q=retrofit&max=50"
curl ".../search?q=okhttp&max=50"
curl ".../search?q=baseUrl&max=50"
```

### Find hardcoded secrets or tokens
```bash
curl ".../search?q=api_key&max=50"
curl ".../search?q=secret&max=50"
curl ".../search?q=token&max=50"
curl ".../search?q=password&max=50"
curl ".../search?q=Bearer&max=50"
curl ".../search?q=AKIA&max=50"   # AWS access key prefix
```

### Review permissions and manifest
```bash
# Read the decoded AndroidManifest
curl ".../file?path=apktool/AndroidManifest.xml"
# Or check the parsed summary
curl "https://coordinator.apps.blj.wtf/results/<job_id>"
# Look at the metadata.permissions array in the results response
```

### Examine app structure
```bash
# Start with the top-level jadx output
curl ".../tree?path=jadx/sources"
# Find the main package
curl ".../tree?path=jadx/sources/com/example"
# Read the entry-point activity
curl ".../file?path=jadx/sources/com/example/MainActivity.java"
```

### Check for obfuscation
Browse `jadx/sources/` — if class/method/field names are single letters (a, b,
c) or short random strings, ProGuard or R8 obfuscation is active. In that case:
- Use search to find meaningful string literals rather than navigating by class name
- Fall back to smali for accurate control-flow analysis

### Find native libraries
```bash
curl ".../tree?path=apktool/lib"
# Lists architectures (arm64-v8a, armeabi-v7a, x86_64, etc.) and .so files
```

### Review MobSF security report
```bash
# Full results including the MobSF report JSON
curl "https://coordinator.apps.blj.wtf/results/<job_id>"
# The mobsf.report field contains the complete MobSF JSON report,
# including security score, findings, and API analysis
```

---

## Listing All Jobs

```bash
# Most recent 50 jobs
curl https://coordinator.apps.blj.wtf/jobs

# With pagination
curl "https://coordinator.apps.blj.wtf/jobs?limit=20&offset=40"
```

Each entry includes `job_id`, `status`, `package_name`, `version`, `source`,
`sha256`, `submitted_at`, `completed_at`, and per-tool statuses.

The web dashboard at **https://coordinator.apps.blj.wtf/** shows the same data
with live auto-refresh every 5 seconds.

---

## Deduplication

If the same APK (identified by SHA256) has already been fully analysed, the
pipeline returns a new `job_id` that is immediately `complete` with the existing
results — no re-analysis is triggered. You can poll the new `job_id` normally.

---

## Tips

- **jadx vs apktool:** jadx output is decompiled Java — easier to read but may
  have decompilation artifacts or missing code. apktool output is smali (Dalvik
  bytecode disassembly) — more accurate but verbose. Start with jadx for
  readability; fall back to smali when jadx output is incomplete or confusing.

- **Large apps:** A complex app may have thousands of source files. Use search
  first to locate areas of interest rather than browsing the tree blindly.

- **Truncated files:** If a file returns `X-Truncated: true`, only the first
  100 KB was returned. For large files, use search to locate specific strings
  within them.

- **Path encoding:** URL-encode the `path` and `q` query parameters when they
  contain special characters (e.g., `https://` → `https%3A%2F%2F`).

- **Health checks:**
  ```bash
  curl https://ingestion.apps.blj.wtf/health
  curl https://coordinator.apps.blj.wtf/health
  ```
