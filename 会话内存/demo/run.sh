#!/bin/bash
# gcdeadtrace demo runner — runs specified mode(s), captures stdout+stderr,
# then prints a verification summary.
#
# Usage:
#   ./run.sh                         # run all modes (default 10s)
#   ./run.sh reuse                   # run only reuse mode, 10s
#   ./run.sh session 5s              # run session mode, 5 seconds
#   ./run.sh all 30s                 # run all modes, 30 seconds

set -euo pipefail

DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="$DEMO_DIR/output"
MODE="${1:-all}"
DURATION="${2:-10s}"

mkdir -p "$OUTPUT_DIR"

echo "=============================================="
echo "  gcdeadtrace Demo Runner"
echo "  Mode:     $MODE"
echo "  Duration: $DURATION"
echo "  Output:   $OUTPUT_DIR"
echo "=============================================="
echo

# Build unique output filenames
OUT_STDOUT="$OUTPUT_DIR/${MODE}_stdout.log"
OUT_STDERR="$OUTPUT_DIR/${MODE}_stderr.log"

echo "Running: GODEBUG=gcdeadtrace=1 go run main.go -mode $MODE -duration $DURATION"
echo

GODEBUG=gcdeadtrace=1 GOROOT=/d/code/go/go /d/code/go/go/bin/go run "$DEMO_DIR/main.go" \
    -mode "$MODE" -duration "$DURATION" \
    > "$OUT_STDOUT" 2> "$OUT_STDERR" || true

# --- Verification Summary ---
echo
echo "=============================================="
echo "  Verification Summary: $MODE"
echo "=============================================="

check() {
    if [ "$2" -gt 0 ]; then
        echo "  [PASS] $1"
    else
        echo "  [FAIL] $1"
    fi
}

case "$MODE" in
    reuse)
        GEN0_START=$(grep -c "5001#0 started" "$OUT_STDERR" 2>/dev/null || echo 0)
        GEN1_START=$(grep -c "5001#1 started" "$OUT_STDERR" 2>/dev/null || echo 0)
        GEN2_START=$(grep -c "5001#2 started" "$OUT_STDERR" 2>/dev/null || echo 0)
        GEN1_LINE=$(grep -c "session #5001#1:" "$OUT_STDERR" 2>/dev/null || echo 0)
        GEN2_LINE=$(grep -c "session #5001#2:" "$OUT_STDERR" 2>/dev/null || echo 0)

        check "gen 0 started (#0)" "$GEN0_START"
        check "gen 1 started (#1)" "$GEN1_START"
        check "gen 2+ started (#2+)" "$GEN2_START"
        check "per-session line shows #1" "$GEN1_LINE"
        check "per-session line shows #2" "$GEN2_LINE"
        ;;
    session)
        SESS_COUNT=$(grep -c "gcdeadsession by session:" "$OUT_STDERR" 2>/dev/null || echo 0)
        check "session breakdown output" "$SESS_COUNT"
        ;;
    concurrent)
        SESSION3010=$(grep -c "session #3001:" "$OUT_STDERR" 2>/dev/null || echo 0)
        check "concurrent session output" "$SESSION3010"
        ;;
    *)
        TOTAL_GC=$(grep -c "=== GC #" "$OUT_STDERR" 2>/dev/null || echo 0)
        TOTAL_SESS=$(grep -c "gcdeadsession by session:" "$OUT_STDERR" 2>/dev/null || echo 0)
        check "any GC output" "$TOTAL_GC"
        check "any session breakdown" "$TOTAL_SESS"
        ;;
esac

echo
echo "  Stdout: $(wc -l < "$OUT_STDOUT") lines"
echo "  Stderr: $(wc -l < "$OUT_STDERR") lines ($(du -h "$OUT_STDERR" | cut -f1))"
echo "=============================================="
