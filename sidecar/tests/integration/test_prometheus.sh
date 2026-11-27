#!/usr/bin/env bash
set -uo pipefail

# test_prometheus.sh — Test Prometheus /metrics endpoint.
# Returns exit code = number of failures.

PROM_URL="http://localhost:${PROM_PORT:-19187}/metrics"
FAILURES=0

pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1 — $2"; FAILURES=$((FAILURES + 1)); }

echo "Testing Prometheus metrics at $PROM_URL"
echo ""

# ---------------------------------------------------------------
# Fetch metrics page
# ---------------------------------------------------------------
METRICS=$(curl -s -w "\n__HTTP__%{http_code}" "$PROM_URL" 2>/dev/null)
HTTP_CODE=$(echo "$METRICS" | grep '__HTTP__' | sed 's/__HTTP__//')
METRICS_BODY=$(echo "$METRICS" | grep -v '__HTTP__')

if [[ "$HTTP_CODE" == "200" ]]; then
    pass "GET /metrics (HTTP $HTTP_CODE)"
else
    fail "GET /metrics" "expected 200, got $HTTP_CODE"
    echo ""
    echo "Prometheus tests complete: $FAILURES failures"
    exit "$FAILURES"
fi

# ---------------------------------------------------------------
# Check for expected metrics
# ---------------------------------------------------------------

check_metric() {
    local metric="$1"
    local label="$2"
    if echo "$METRICS_BODY" | grep -q "$metric"; then
        pass "$label"
    else
        fail "$label" "metric '$metric' not found"
    fi
}

check_metric "pg_sage_info"                      "pg_sage_info present"
check_metric "pg_sage_mode"                      "pg_sage_mode present"
check_metric "pg_sage_connection_up"             "pg_sage_connection_up present"
check_metric "pg_sage_findings_open"             "pg_sage_findings_open present"
check_metric "pg_sage_collector_last_success_timestamp_seconds" \
    "pg_sage_collector_last_success_timestamp_seconds present"
check_metric "pg_sage_database_size_bytes"       "pg_sage_database_size_bytes present"

# ---------------------------------------------------------------
# Check for LLM metrics (should show pg_sage_llm_enabled)
# ---------------------------------------------------------------
check_metric "pg_sage_llm_enabled"               "pg_sage_llm_enabled present"

# ---------------------------------------------------------------
# Check optimizer metric
# ---------------------------------------------------------------
check_metric "pg_sage_optimizer_enabled"         "pg_sage_optimizer_enabled present"

# ---------------------------------------------------------------
# Verify severity labels on findings
# ---------------------------------------------------------------
if echo "$METRICS_BODY" | grep -q 'pg_sage_findings_open{database=.*severity='; then
    pass "pg_sage_findings_open has database and severity labels"
else
    fail "pg_sage_findings_open labels" "expected database and severity labels"
fi

# ---------------------------------------------------------------
# Verify connection_up = 1
# ---------------------------------------------------------------
CONN_VAL=$(echo "$METRICS_BODY" | grep '^pg_sage_connection_up ' | awk '{print $2}')
if [[ "$CONN_VAL" == "1" ]]; then
    pass "pg_sage_connection_up = 1"
else
    fail "pg_sage_connection_up value" "expected 1, got '$CONN_VAL'"
fi

# ---------------------------------------------------------------
# Verify mode = 1 (standalone)
# ---------------------------------------------------------------
MODE_VAL=$(echo "$METRICS_BODY" | grep '^pg_sage_mode ' | awk '{print $2}')
if [[ "$MODE_VAL" == "1" ]]; then
    pass "pg_sage_mode = 1 (standalone)"
else
    fail "pg_sage_mode value" "expected 1, got '$MODE_VAL'"
fi

echo ""
echo "Prometheus tests complete: $FAILURES failures"
exit "$FAILURES"
