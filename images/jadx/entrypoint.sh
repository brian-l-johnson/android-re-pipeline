#!/bin/sh
set -e

APK=""
OUTPUT=""

# Parse arguments
while [ $# -gt 0 ]; do
    case "$1" in
        --apk)
            APK="$2"
            shift 2
            ;;
        --output)
            OUTPUT="$2"
            shift 2
            ;;
        *)
            echo "Unknown argument: $1" >&2
            echo "Usage: $0 --apk <path> --output <path>" >&2
            exit 1
            ;;
    esac
done

if [ -z "$APK" ]; then
    echo "Error: --apk <path> is required" >&2
    exit 1
fi

if [ -z "$OUTPUT" ]; then
    echo "Error: --output <path> is required" >&2
    exit 1
fi

if [ ! -f "$APK" ]; then
    echo "Error: APK file not found: $APK" >&2
    exit 1
fi

echo "Starting jadx decompilation..."
echo "  APK:    $APK"
echo "  Output: $OUTPUT"

# jadx tries to create config/plugin dirs relative to HOME or XDG dirs.
# In a read-only-root non-root container these must be redirected to /tmp.
export HOME=/tmp
export XDG_CONFIG_HOME=/tmp/.config
export XDG_DATA_HOME=/tmp/.local/share
export XDG_CACHE_HOME=/tmp/.cache

# Cap JVM heap to leave headroom within the container memory limit.
# JADX_OPTS is passed directly to the JVM by the jadx launcher script.
export JADX_OPTS="-Xmx3g"

/opt/jadx/bin/jadx --output-dir "$OUTPUT" "$APK" || true
# jadx exits non-zero when some classes fail to decompile, even if the
# majority of output was produced successfully. Treat the job as a success
# if any output files were written; fail only if no output was produced at all.
FILE_COUNT=$(find "$OUTPUT" -type f 2>/dev/null | wc -l | tr -d ' ')
DIR_COUNT=$(find "$OUTPUT" -type d 2>/dev/null | wc -l | tr -d ' ')

if [ "$FILE_COUNT" -eq 0 ]; then
    echo "ERROR: jadx produced no output files - treating as failure." >&2
    exit 1
fi

echo "Decompilation complete (partial errors are normal)."
echo "  Files:       $FILE_COUNT"
echo "  Directories: $DIR_COUNT"
exit 0
