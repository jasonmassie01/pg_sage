#!/usr/bin/env bash
set -euo pipefail

# run_tests.sh — pg_sage sidecar integration test orchestrator.
# Usage: ./run_tests.sh <platform> <pg_host> <pg_port> <pg_user> <pg_password> <pg_database> [gemini_key]
#
# Example:
#   ./run_tests.sh local localhost 5432 sage_agent secret sage_test
#   ./run_tests.sh cloud-sql 10.0.0.5 5432 sage_agent secret sage_test sk-xxx

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SIDECAR_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
PSQL="${PSQL:-psql}"
SIDECAR_PID=""
SIDECAR_LOG="$SCRIPT_DIR/sidecar.log"
PASS_COUNT=0
FAIL_COUNT=0

cleanup() {
    if [[ -n "$SIDECAR_PID" ]] && kill -0 "$SIDECAR_PID" 2>/dev/null; then
        echo "==> Stopping sidecar (PID $SIDECAR_PID)..."
        kill "$SIDECAR_PID" 2>/dev/null || true
        wait "$SIDECAR_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

usage() {
    echo "Usage: $0 <platform> <pg_host> <pg_port> <pg_user> <pg_password> <pg_database> [gemini_key]"
    echo ""
    echo "  platform   — label for results file (local, cloud-sql, rds, aurora, azure)"
    echo "  gemini_key — optional; enables LLM features"
    exit 1
}

if [[ $# -lt 6 ]]; then
    usage
fi

PLATFORM="$1"
export SAGE_PG_HOST="$2"
export SAGE_PG_PORT="$3"
export SAGE_PG_USER="$4"
export SAGE_PG_PASSWORD="$5"
export SAGE_PG_DATABASE="$6"

if [[ $# -ge 7 && -n "${7:-}" ]]; then
    export SAGE_GEMINI_API_KEY="$7"
    export SAGE_LLM_API_KEY="$7"
    echo "==> LLM enabled (Gemini key provided)"
else
    export SAGE_GEMINI_API_KEY=""
    export SAGE_LLM_API_KEY=""
    echo "==> LLM disabled (no Gemini key)"
fi

PGPASSWORD="$SAGE_PG_PASSWORD"
export PGPASSWORD

echo "============================================================"
echo " pg_sage integration tests — platform: $PLATFORM"
echo " target: $SAGE_PG_HOST:$SAGE_PG_PORT/$SAGE_PG_DATABASE"
echo " user:   $SAGE_PG_USER"
echo " time:   $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "============================================================"
echo ""

# ---------------------------------------------------------------
# Step 1: Build sidecar binary
# ---------------------------------------------------------------
echo "==> Building sidecar binary..."
cd "$SIDECAR_DIR"
go build -o "$SCRIPT_DIR/pg_sage_sidecar" ./cmd/pg_sage_sidecar/
echo "    Built: $SCRIPT_DIR/pg_sage_sidecar"

# ---------------------------------------------------------------
# Step 2: Seed test objects via psql
# ---------------------------------------------------------------
echo "==> Seeding test objects (seed_objects.sql)..."
"$PSQL" -h "$SAGE_PG_HOST" -p "$SAGE_PG_PORT" \
    -U "$SAGE_PG_USER" -d "$SAGE_PG_DATABASE" \
    -f "$SCRIPT_DIR/seed_objects.sql" \
    --set ON_ERROR_STOP=1 -q
echo "    Seed objects loaded."

# ---------------------------------------------------------------
# Step 3: Start sidecar in background
# ---------------------------------------------------------------
echo "==> Starting sidecar with config_test.yaml..."
"$SCRIPT_DIR/pg_sage_sidecar" \
    -config "$SCRIPT_DIR/config_test.yaml" \
    > "$SIDECAR_LOG" 2>&1 &
SIDECAR_PID=$!
echo "    Sidecar PID: $SIDECAR_PID"

# ---------------------------------------------------------------
# Step 4: Wait for startup
# ---------------------------------------------------------------
echo "==> Waiting 10s for sidecar startup..."
sleep 10

if ! kill -0 "$SIDECAR_PID" 2>/dev/null; then
    echo "FATAL: Sidecar exited during startup. Last 30 lines of log:"
    tail -30 "$SIDECAR_LOG"
    exit 1
fi
echo "    Sidecar is running."

# ---------------------------------------------------------------
# Step 5: Seed historical snapshots (after schema bootstrap)
# ---------------------------------------------------------------
echo "==> Seeding historical snapshots (seed_snapshots.sql)..."
"$PSQL" -h "$SAGE_PG_HOST" -p "$SAGE_PG_PORT" \
    -U "$SAGE_PG_USER" -d "$SAGE_PG_DATABASE" \
    -f "$SCRIPT_DIR/seed_snapshots.sql" \
    --set ON_ERROR_STOP=1 -q
echo "    Historical snapshots loaded."

# ---------------------------------------------------------------
# Step 6: Wait for collector/analyzer cycles
# ---------------------------------------------------------------
echo "==> Waiting 90s for 3+ collector and 2+ analyzer cycles..."
sleep 90

if ! kill -0 "$SIDECAR_PID" 2>/dev/null; then
    echo "FATAL: Sidecar exited during test window. Last 30 lines of log:"
    tail -30 "$SIDECAR_LOG"
    exit 1
fi
echo "    Collection window complete."

# ---------------------------------------------------------------
# Step 7: Run test suites
# ---------------------------------------------------------------
echo ""
echo "============================================================"
echo " Running test suites"
echo "============================================================"

export API_PORT=18080
export PROM_PORT=19187

run_suite() {
    local name="$1"
    local script="$2"
    echo ""
    echo "--- $name ---"
    set +e
    bash "$script"
    local rc=$?
    set -e
    if [[ $rc -eq 0 ]]; then
        echo "--- $name: ALL PASSED ---"
    else
        echo "--- $name: $rc FAILURES ---"
        FAIL_COUNT=$((FAIL_COUNT + rc))
    fi
    return 0
}

run_suite "API Tests"        "$SCRIPT_DIR/test_api.sh"
run_suite "Prometheus Tests"  "$SCRIPT_DIR/test_prometheus.sh"
run_suite "Verify Rules"     "$SCRIPT_DIR/verify.sh"

# ---------------------------------------------------------------
# Step 8: Stop sidecar
# ---------------------------------------------------------------
echo ""
echo "==> Stopping sidecar..."
kill "$SIDECAR_PID" 2>/dev/null || true
wait "$SIDECAR_PID" 2>/dev/null || true
SIDECAR_EXIT=$?
SIDECAR_PID=""
echo "    Sidecar exit code: $SIDECAR_EXIT"

# ---------------------------------------------------------------
# Step 9: Summary
# ---------------------------------------------------------------
echo ""
echo "============================================================"
echo " Integration Test Summary — $PLATFORM"
echo "============================================================"
echo " Failures: $FAIL_COUNT"
if [[ $FAIL_COUNT -eq 0 ]]; then
    echo " Result:   PASS"
else
    echo " Result:   FAIL"
fi
echo "============================================================"

# ---------------------------------------------------------------
# Step 10: Save results
# ---------------------------------------------------------------
RESULTS_FILE="$SCRIPT_DIR/results_${PLATFORM}.md"
cat > "$RESULTS_FILE" <<RESULTS_EOF
# pg_sage Integration Test Results — $PLATFORM

- **Date:** $(date -u '+%Y-%m-%dT%H:%M:%SZ')
- **Target:** $SAGE_PG_HOST:$SAGE_PG_PORT/$SAGE_PG_DATABASE
- **User:** $SAGE_PG_USER
- **LLM:** $(if [[ -n "$SAGE_GEMINI_API_KEY" ]]; then echo "enabled (gemini-2.0-flash)"; else echo "disabled"; fi)
- **Sidecar Exit:** $SIDECAR_EXIT
- **Failures:** $FAIL_COUNT
- **Result:** $(if [[ $FAIL_COUNT -eq 0 ]]; then echo "PASS"; else echo "FAIL"; fi)

## Sidecar Log (last 50 lines)

\`\`\`
$(tail -50 "$SIDECAR_LOG")
\`\`\`
RESULTS_EOF

echo "Results saved to: $RESULTS_FILE"

exit "$FAIL_COUNT"
