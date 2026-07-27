#!/bin/bash
# Skill script: run gcdeadtrace demo
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/../../../../" && pwd)"
DEMO_DIR="$PROJECT_DIR/会话内存/demo"
OUTPUT_DIR="$DEMO_DIR/output"

MODE="${1:-all}"
DURATION="${2:-10s}"

mkdir -p "$OUTPUT_DIR"

OUT_STDOUT="$OUTPUT_DIR/${MODE}_stdout.log"
OUT_STDERR="$OUTPUT_DIR/${MODE}_stderr.log"

echo "Running gcdeadtrace demo (mode=$MODE, duration=$DURATION)..."
echo "  stdout -> $OUT_STDOUT"
echo "  stderr -> $OUT_STDERR"
echo

GODEBUG=gcdeadtrace=1 GOROOT=/d/code/go/go /d/code/go/go/bin/go run "$DEMO_DIR/main.go" \
    -mode "$MODE" -duration "$DURATION" \
    > "$OUT_STDOUT" 2> "$OUT_STDERR" || true

# Verification summary
echo "=============================================="
echo "  Verification Summary: $MODE"
echo "=============================================="

verify() {
    local label="$1" pattern="$2" file="$3"
    local count
    count=$(grep -c "$pattern" "$file" 2>/dev/null || true)
    if [ "$count" -gt 0 ]; then
        echo "  [PASS] $label"
    else
        echo "  [FAIL] $label"
    fi
}

case "$MODE" in
    reuse)
        verify "gen 0 started (#0)"  "5001#0 started"  "$OUT_STDERR"
        verify "gen 1 started (#1)"  "5001#1 started"  "$OUT_STDERR"
        verify "gen 2+ started (#2+)" "5001#2 started" "$OUT_STDERR"
        verify "per-session line #1" "session #5001#1:" "$OUT_STDERR"
        verify "per-session line #2" "session #5001#2:" "$OUT_STDERR"
        ;;
    session|concurrent|loop|mixed|fullydead|customtypes|reprint)
        verify "session breakdown output" "gcdeadsession by session:" "$OUT_STDERR"
        verify "freed output present"      "gcdeadsession:freed:" "$OUT_STDERR"
        verify "alive output present"      "gcdeadsession:alive:" "$OUT_STDERR"
        ;;
    *)
        verify "any GC output"            "=== GC #" "$OUT_STDERR"
        verify "any session breakdown"    "gcdeadsession by session:" "$OUT_STDERR"
        ;;
esac

echo
# Check demo OK
if grep -q "Demo finished" "$OUT_STDOUT" 2>/dev/null; then
    echo "  [INFO] Demo completed normally"
fi
echo "  Lines: stdout=$(wc -l < "$OUT_STDOUT") stderr=$(wc -l < "$OUT_STDERR")"
echo "=============================================="
