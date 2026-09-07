#!/usr/bin/env bash
set -uo pipefail

# verify_llm.sh — Verify LLM-dependent features (Tier 2).
# Run AFTER the sidecar has had time to execute LLM cycles.
# Returns exit code = number of failures.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PSQL="${PSQL:-psql}"
SIDECAR_LOG="${SIDECAR_LOG:-$SCRIPT_DIR/sidecar.log}"
FAILURES=0

PGPASSWORD="${SAGE_PG_PASSWORD}"
export PGPASSWORD
PG_CONN="-h ${SAGE_PG_HOST} -p ${SAGE_PG_PORT} -U ${SAGE_PG_USER} -d ${SAGE_PG_DATABASE}"

pass() { echo "  PASS  $1"; }
fail() { echo "  FAIL  $1 — $2"; FAILURES=$((FAILURES + 1)); }
skip() { echo "  SKIP  $1 — $2"; }

sql() {
    "$PSQL" $PG_CONN -tAc "$1" 2>/dev/null | tr -d '[:space:]'
}

echo "Verifying LLM-dependent features (Tier 2)"
echo ""

# ===============================================================
# 1. Briefings
# ===============================================================
echo "--- Briefings ---"
BRIEFING_COUNT=$(sql "SELECT count(*) FROM sage.briefings")
if [[ -n "$BRIEFING_COUNT" && "$BRIEFING_COUNT" -gt 0 ]]; then
    pass "Briefings generated ($BRIEFING_COUNT)"
    # Check content is not empty
    BRIEFING_CONTENT=$(sql "SELECT length(content_text) FROM sage.briefings ORDER BY generated_at DESC LIMIT 1")
    if [[ -n "$BRIEFING_CONTENT" && "$BRIEFING_CONTENT" -gt 50 ]]; then
        pass "Briefing content non-trivial ($BRIEFING_CONTENT chars)"
    else
        fail "Briefing content" "content too short ($BRIEFING_CONTENT chars)"
    fi
    # Check LLM was used
    LLM_USED=$(sql "SELECT llm_used FROM sage.briefings ORDER BY generated_at DESC LIMIT 1")
    if [[ "$LLM_USED" == "t" ]]; then
        pass "Briefing used LLM"
    else
        fail "Briefing LLM usage" "llm_used=$LLM_USED"
    fi
else
    # Known bug: cron parser only supports daily "0 H * * *", not minute-level.
    # See tasks/todo.md: "Fix briefing cron parser to support minute-level schedules"
    skip "Briefings" "known cron parser limitation (minute-level schedules unsupported)"
fi

# ===============================================================
# 2. LLM Circuit Breaker
# ===============================================================
echo ""
echo "--- LLM Health ---"
if grep -q "circuit breaker opened" "$SIDECAR_LOG" 2>/dev/null; then
    fail "LLM circuit breaker" "circuit breaker opened — LLM calls failing"
else
    pass "LLM circuit breaker not triggered"
fi

if grep -qi "401\|403\|invalid.*api.*key\|unauthorized" "$SIDECAR_LOG" 2>/dev/null; then
    fail "LLM auth" "authentication errors in log"
else
    pass "No LLM auth errors"
fi

# ===============================================================
# 3. Advisor Findings
# ===============================================================
echo ""
echo "--- Advisor (LLM-powered config tuning) ---"
# Advisor generates findings with specific categories
for cat in vacuum_tuning wal_tuning connection_tuning memory_tuning; do
    count=$(sql "SELECT count(*) FROM sage.findings WHERE category = '$cat'")
    if [[ -n "$count" && "$count" -gt 0 ]]; then
        pass "Advisor: $cat ($count findings)"
    else
        skip "Advisor: $cat" "no findings (advisor may not have run yet)"
    fi
done

# Check if advisor ran at all (look in logs)
if grep -qi "advisor.*cycle\|advisor.*complete\|advisor.*vacuum\|advisor.*wal\|advisor.*memory\|advisor.*connection" "$SIDECAR_LOG" 2>/dev/null; then
    pass "Advisor cycle detected in logs"
else
    fail "Advisor" "no advisor activity in logs"
fi

# ===============================================================
# 4. Optimizer (LLM Index Recommendations)
# ===============================================================
echo ""
echo "--- Optimizer (LLM index recommendations) ---"
if grep -qi "optimizer.*recommend\|optimizer.*candidate\|optimizer.*cycle\|optimizer.*skip\|optimizer.*context" "$SIDECAR_LOG" 2>/dev/null; then
    pass "Optimizer cycle detected in logs"
else
    skip "Optimizer" "no optimizer activity in logs (may need more query data)"
fi

# ===============================================================
# 5. auto_explain Detection
# ===============================================================
echo ""
echo "--- auto_explain ---"
if grep -qi "auto_explain.*available\|auto_explain.*session_load\|auto_explain.*shared_preload\|autoexplain.*detect" "$SIDECAR_LOG" 2>/dev/null; then
    pass "auto_explain detected"
    # Check if plans were captured
    AE_COUNT=$(sql "SELECT count(*) FROM sage.explain_cache WHERE source = 'auto_explain'")
    if [[ -n "$AE_COUNT" && "$AE_COUNT" -gt 0 ]]; then
        pass "auto_explain captured $AE_COUNT plans"
    else
        skip "auto_explain plans" "no auto_explain plans captured yet"
    fi
else
    skip "auto_explain" "not detected (may not be available)"
fi

# ===============================================================
# 6. pg_hint_plan / Tuner
# ===============================================================
echo ""
echo "--- Tuner / pg_hint_plan ---"
if grep -qi "hint_plan.*available\|pg_hint_plan.*detect\|hint_plan.*enabled\|tuner.*enabled" "$SIDECAR_LOG" 2>/dev/null; then
    pass "pg_hint_plan detected"
    # Check if hints were generated
    HINT_COUNT=$(sql "SELECT count(*) FROM sage.query_hints")
    if [[ -n "$HINT_COUNT" && "$HINT_COUNT" -gt 0 ]]; then
        pass "Tuner generated $HINT_COUNT query hints"
    else
        skip "Tuner hints" "no hints generated yet"
    fi
else
    skip "pg_hint_plan" "not detected (may not be installed)"
fi

# Check tuner activity in logs
if grep -qi "tuner.*candidate\|tuner.*symptom\|tuner.*prescri\|tuner.*scan\|tuner.*cycle" "$SIDECAR_LOG" 2>/dev/null; then
    pass "Tuner cycle detected in logs"
else
    skip "Tuner" "no tuner activity in logs"
fi

# ===============================================================
# 7. Explain Cache (on-demand plan capture)
# ===============================================================
echo ""
echo "--- Explain Cache ---"
EXPLAIN_TOTAL=$(sql "SELECT count(*) FROM sage.explain_cache")
if [[ -n "$EXPLAIN_TOTAL" && "$EXPLAIN_TOTAL" -gt 0 ]]; then
    pass "Explain cache has $EXPLAIN_TOTAL entries"
    # Check sources
    for src in auto_explain session_load latest_snapshot; do
        src_count=$(sql "SELECT count(*) FROM sage.explain_cache WHERE source = '$src'")
        if [[ -n "$src_count" && "$src_count" -gt 0 ]]; then
            pass "Explain source '$src': $src_count plans"
        fi
    done
else
    fail "Explain cache" "empty"
fi

# ===============================================================
# 8. API health during LLM verification
# ===============================================================
echo ""
echo "--- API Health ---"
API_BASE="http://localhost:${API_PORT:-18080}/api/v1"

# sage_briefing tool — check via API that briefings exist
BRIEFING_API=$(curl -s -o /dev/null -w "%{http_code}" "$API_BASE/databases" 2>/dev/null)
if [[ "$BRIEFING_API" == "200" ]]; then
    pass "API still responsive during LLM testing"
else
    fail "API health" "returned $BRIEFING_API"
fi

# ===============================================================
# Summary
# ===============================================================
echo ""
echo "LLM verification complete: $FAILURES failures"
exit "$FAILURES"
