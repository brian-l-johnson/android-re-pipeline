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

/opt/jadx/bin/jadx --output-dir "$OUTPUT" "$APK"
JADX_EXIT=$?

if [ $JADX_EXIT -eq 0 ]; then
    FILE_COUNT=$(find "$OUTPUT" -type f | wc -l | tr -d ' ')
    DIR_COUNT=$(find "$OUTPUT" -type d | wc -l | tr -d ' ')
    echo "Decompilation complete."
    echo "  Files:       $FILE_COUNT"
    echo "  Directories: $DIR_COUNT"
fi

exit $JADX_EXIT
