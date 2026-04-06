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

echo "Starting apktool decode..."
echo "  APK:    $APK"
echo "  Output: $OUTPUT"

java -jar /opt/apktool.jar d -f --frame-path /tmp/apktool-framework -o "$OUTPUT" "$APK"
exit $?
