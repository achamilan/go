#!/bin/bash
# Run gcdeadtrace demo in each mode separately.
# Each mode outputs gcdeadtrace data to a separate file.
# Output files are placed in the output/ subdirectory.
#
# NOTE: The gcdeadtracefile GODEBUG parameter uses Windows CreateFileA (ANSI),
# which does not support paths containing non-ASCII characters (e.g. Chinese).
# Therefore we use a relative path from the repo root.
#
# Usage: bash run_modes.sh

set -e

DEMO_DIR="D:/code/go/go/会话内存/demo"
OUTPUT_DIR="$DEMO_DIR/output"
REPO_ROOT="D:/code/go/go"
DEMO_EXE="$DEMO_DIR/demo.exe"

mkdir -p "$OUTPUT_DIR"

# Ensure the demo is built
if [ ! -f "$DEMO_EXE" ]; then
    echo "Building demo..."
    cd "$DEMO_DIR"
    GOROOT="$REPO_ROOT" "$REPO_ROOT/bin/go" build -o demo.exe main.go
fi

# Only session, fullydead, and concurrent patterns use GcDeadSessionStart/End
# and produce gcdeadtrace output. loop and mixed patterns do not use sessions.
modes_with_session=("session" "fullydead" "concurrent" "customtypes")
modes_without_session=("loop" "mixed")

echo "========================================"
echo "Modes that use GcDeadSession (produce output):"
echo "  ${modes_with_session[*]}"
echo "Modes without sessions (no output):"
echo "  ${modes_without_session[*]}"
echo "========================================"
echo ""

for mode in "${modes_with_session[@]}"; do
    output_name="gcdeadtrace_demo_${mode}.txt"
    output_file="$OUTPUT_DIR/$output_name"
    temp_file="${output_name}"

    echo "Running mode: $mode -> $output_file"

    cd "$REPO_ROOT"
    GODEBUG=gcdeadtrace=1,gcdeadtracefile="$temp_file" \
        "$DEMO_EXE" -mode="$mode" -duration=2s 2>/dev/null

    if [ -f "$REPO_ROOT/$temp_file" ]; then
        mv "$REPO_ROOT/$temp_file" "$output_file"
        size=$(wc -c < "$output_file")
        echo "  Output: $size bytes"
    else
        echo "  WARNING: No output file produced"
    fi

    sleep 1
done

for mode in "${modes_without_session[@]}"; do
    echo "Running mode: $mode (no gcdeadtrace output expected)"
    cd "$REPO_ROOT"
    GODEBUG=gcdeadtrace=1 "$DEMO_EXE" -mode="$mode" -duration=2s 2>/dev/null
    sleep 1
done

echo ""
echo "========================================"
echo "Output files:"
echo "========================================"
ls -la "$OUTPUT_DIR"/
