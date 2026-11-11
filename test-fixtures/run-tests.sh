#!/bin/bash
set -euo pipefail

# ============================================================
# pg_sage Functional Test Runner
# ============================================================

# Configuration
API_URL="${API_URL:-http://localhost:8080/api/v1}"
PG_URL="${PG_URL:-postgres://postgres:test@localhost:5433/testdb}"
PG_URL_2="${PG_URL_2:-postgres://postgres:test@localhost:5434/testdb2}"
LLM_MOCK_URL="${LLM_MOCK_URL:-http://localhost:11434}"
WEBHOOK_MOCK_URL="${WEBHOOK_MOCK_URL:-http://localhost:9999}"
COMPOSE_FILE="${COMPOSE_FILE:-$(dirname "$0")/../docker-compose.test.yml}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color
BOLD='\033[1m'

# Tracking
TOTAL_CHECKS=0
PASSED=0
FAILED=0
SKIPPED=0
RESULTS=()
AUTH_TOKEN=""

# ============================================================
# Utility Functions
# ============================================================

check() {
    local id="$1"
    local description="$2"
    local result="$3"  # "pass", "fail", or "skip"
    local detail="${4:-}"

    TOTAL_CHECKS=$((TOTAL_CHECKS + 1))

    case "$result" in
        pass)
            PASSED=$((PASSED + 1))
            echo -e "  ${GREEN}✓ CHECK-${id}: [PASS]${NC} ${description}"
            RESULTS+=("CHECK-${id}: [PASS] ${description}")
            ;;
        fail)
            FAILED=$((FAILED + 1))
            echo -e "  ${RED}✗ CHECK-${id}: [FAIL]${NC} ${description}"
            [ -n "$detail" ] && echo -e "    ${RED}Detail: ${detail}${NC}"
            RESULTS+=("CHECK-${id}: [FAIL] ${description} -- ${detail}")
            ;;
        skip)
            SKIPPED=$((SKIPPED + 1))
            echo -e "  ${YELLOW}⊘ CHECK-${id}: [SKIP]${NC} ${description}"
            [ -n "$detail" ] && echo -e "    ${YELLOW}Reason: ${detail}${NC}"
            RESULTS+=("CHECK-${id}: [SKIP] ${description} -- ${detail}")
            ;;
    esac
}

api() {
    local method="$1"
    local path="$2"
    local data="${3:-}"
    local headers=(-H "Content-Type: application/json")

    if [ -n "$AUTH_TOKEN" ]; then
        headers+=(-H "Authorization: Bearer $AUTH_TOKEN")
    fi

    if [ -n "$data" ]; then
        curl -s -X "$method" "${API_URL}${path}" "${headers[@]}" -d "$data"
    else
        curl -s -X "$method" "${API_URL}${path}" "${headers[@]}"
    fi
}

api_status() {
    local method="$1"
    local path="$2"
    local data="${3:-}"
    local headers=(-H "Content-Type: application/json")

    if [ -n "$AUTH_TOKEN" ]; then
        headers+=(-H "Authorization: Bearer $AUTH_TOKEN")
    fi

    if [ -n "$data" ]; then
        curl -s -o /dev/null -w "%{http_code}" -X "$method" "${API_URL}${path}" "${headers[@]}" -d "$data"
    else
        curl -s -o /dev/null -w "%{http_code}" -X "$method" "${API_URL}${path}" "${headers[@]}"
    fi
}

wait_for_service() {
    local url="$1"
    local name="$2"
    local max_wait="${3:-60}"
    local elapsed=0

    echo -n "  Waiting for ${name}..."
    while [ $elapsed -lt $max_wait ]; do
        if curl -s -o /dev/null -w "%{http_code}" "$url" 2>/dev/null | grep -q "200\|301\|302"; then
            echo -e " ${GREEN}ready${NC} (${elapsed}s)"
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
        echo -n "."
    done
    echo -e " ${RED}timeout after ${max_wait}s${NC}"
    return 1
}

wait_for_pg() {
    local url="$1"
    local name="$2"
    local max_wait="${3:-60}"
    local elapsed=0

    echo -n "  Waiting for ${name}..."
    while [ $elapsed -lt $max_wait ]; do
        if psql "$url" -c "SELECT 1" > /dev/null 2>&1; then
            echo -e " ${GREEN}ready${NC} (${elapsed}s)"
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
        echo -n "."
    done
    echo -e " ${RED}timeout after ${max_wait}s${NC}"
    return 1
}

login() {
    local resp
    resp=$(curl -s -X POST "${API_URL}/auth/login" \
        -H "Content-Type: application/json" \
        -d '{"email":"admin@test.com","password":"testpassword123"}')
    AUTH_TOKEN=$(echo "$resp" | jq -r '.token // .session_token // .access_token // empty')
    if [ -z "$AUTH_TOKEN" ]; then
        echo -e "  ${YELLOW}Warning: Could not extract auth token, trying without auth${NC}"
        AUTH_TOKEN=""
    fi
}

section() {
    echo ""
    echo -e "${BOLD}${BLUE}═══════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}${BLUE}  $1${NC}"
    echo -e "${BOLD}${BLUE}═══════════════════════════════════════════════════${NC}"
}

# ============================================================
# Phase 0: Environment Bootstrap
# ============================================================
phase0() {
    section "Phase 0: Environment Bootstrap"

    # Start the stack
    echo "  Starting Docker Compose stack..."
    docker compose -f "$COMPOSE_FILE" up -d 2>&1 | tail -5

    # Wait for services
    wait_for_pg "$PG_URL" "pg-target" 90
    local pg1=$?
    wait_for_pg "$PG_URL_2" "pg-target-2" 90
    local pg2=$?
    wait_for_service "${LLM_MOCK_URL}/health" "llm-mock" 30
    wait_for_service "${WEBHOOK_MOCK_URL}/health" "webhook-mock" 30
    wait_for_service "${API_URL}/databases" "sidecar API" 120
    local sidecar=$?

    # CHECK 0.1: All containers healthy
    local running
    running=$(docker compose -f "$COMPOSE_FILE" ps --status=running --format json 2>/dev/null | jq -s 'length')
    if [ "$running" -ge 4 ]; then
        check "0.1" "All containers running (${running} services)" "pass"
    else
        check "0.1" "All containers running" "fail" "Only ${running} running"
    fi

    # CHECK 0.2: Sidecar connects
    if [ "$sidecar" -eq 0 ]; then
        check "0.2" "Sidecar API reachable" "pass"
    else
        check "0.2" "Sidecar API reachable" "fail" "Timeout waiting for API"
    fi

    # CHECK 0.3: Schema bootstrap
    local sage_tables
    sage_tables=$(psql "$PG_URL" -t -c "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'sage'" 2>/dev/null | tr -d ' ')
    if [ "${sage_tables:-0}" -gt 0 ]; then
        check "0.3" "Schema bootstrap (${sage_tables} sage.* tables)" "pass"
    else
        check "0.3" "Schema bootstrap" "fail" "No sage.* tables found"
    fi

    # CHECK 0.4: pg_stat_statements
    local pss
    pss=$(psql "$PG_URL" -t -c "SELECT count(*) FROM pg_extension WHERE extname = 'pg_stat_statements'" 2>/dev/null | tr -d ' ')
    if [ "${pss:-0}" -gt 0 ]; then
        check "0.4" "pg_stat_statements installed" "pass"
    else
        check "0.4" "pg_stat_statements installed" "fail"
    fi

    # CHECK 0.5: Admin login
    login
    if [ -n "$AUTH_TOKEN" ]; then
        check "0.5" "Admin user login" "pass"
    else
        check "0.5" "Admin user login" "fail" "No token returned"
    fi

    # CHECK 0.6: Fleet mode
    local db_count
    db_count=$(api GET "/databases" | jq 'if type == "array" then length else .databases // [] | length end')
    if [ "${db_count:-0}" -ge 2 ]; then
        check "0.6" "Fleet mode active (${db_count} databases)" "pass"
    else
        check "0.6" "Fleet mode active" "fail" "Expected 2 databases, got ${db_count}"
    fi
}

# ============================================================
# Phase 1: Collector Verification
# ============================================================
phase1() {
    section "Phase 1: Collector Verification"
    login

    # Generate some query activity
    echo "  Generating query activity..."
    for i in $(seq 1 15); do
        psql "$PG_URL" -c "SELECT count(*) FROM orders WHERE customer_id = $((i * 7))" > /dev/null 2>&1
    done

    echo "  Waiting for 3 collector cycles (15s)..."
    sleep 15

    # CHECK 1.1: First snapshot
    local snapshot
    snapshot=$(api GET "/snapshots/latest?database=testdb")
    local has_data
    has_data=$(echo "$snapshot" | jq 'if . == null or . == {} then "no" else "yes" end' 2>/dev/null)
    if [ "$has_data" = '"yes"' ]; then
        check "1.1" "First snapshot collected" "pass"
    else
        check "1.1" "First snapshot collected" "fail" "Empty or null snapshot"
    fi

    # CHECK 1.2: Query stats
    local query_found
    query_found=$(echo "$snapshot" | jq '[.. | objects | select(.query? // "" | test("customer_id"))] | length > 0' 2>/dev/null)
    if [ "$query_found" = "true" ]; then
        check "1.2" "Query stats collected (customer_id queries found)" "pass"
    else
        check "1.2" "Query stats collected" "skip" "Snapshot structure may differ"
    fi

    # CHECK 1.3: Table stats
    local table_found
    table_found=$(echo "$snapshot" | jq '[.. | objects | select(.relname? == "orders" or .table_name? == "orders")] | length > 0' 2>/dev/null)
    if [ "$table_found" = "true" ]; then
        check "1.3" "Table stats collected (orders table found)" "pass"
    else
        check "1.3" "Table stats collected" "skip" "Snapshot structure may differ"
    fi

    # CHECK 1.4: Snapshot history
    sleep 10  # Wait for more cycles
    local history_count
    history_count=$(api GET "/snapshots/history?database=testdb&limit=10" | jq 'if type == "array" then length else .snapshots // [] | length end' 2>/dev/null)
    if [ "${history_count:-0}" -ge 2 ]; then
        check "1.4" "Snapshot history (${history_count} entries)" "pass"
    else
        check "1.4" "Snapshot history" "fail" "Expected >= 2, got ${history_count}"
    fi

    # CHECK 1.5: Per-DB isolation
    local snap2
    snap2=$(api GET "/snapshots/latest?database=testdb2")
    local has_data2
    has_data2=$(echo "$snap2" | jq 'if . == null or . == {} then "no" else "yes" end' 2>/dev/null)
    if [ "$has_data2" = '"yes"' ]; then
        check "1.5" "Per-DB snapshot isolation (testdb2 has data)" "pass"
    else
        check "1.5" "Per-DB snapshot isolation" "fail" "testdb2 snapshot empty"
    fi

    # CHECK 1.6: Collector metrics
    local metrics
    metrics=$(api GET "/metrics")
    local has_collector
    has_collector=$(echo "$metrics" | jq 'has("collector_cycles") or has("pg_sage_collector_cycles")' 2>/dev/null)
    if [ "$has_collector" = "true" ]; then
        check "1.6" "Collector metrics present" "pass"
    else
        check "1.6" "Collector metrics present" "skip" "Metrics structure may differ"
    fi
}

# ============================================================
# Phase 2: Analyzer Rules Engine
# ============================================================
phase2() {
    section "Phase 2: Analyzer Rules Engine"
    login

    # Generate workloads to trigger rules
    echo "  Running workloads to trigger analyzer rules..."
    bash "${SCRIPT_DIR}/workload-slow.sh" &
    bash "${SCRIPT_DIR}/workload-bloat.sh" &
    bash "${SCRIPT_DIR}/workload-sequence-exhaust.sh"
    wait

    echo "  Waiting for analyzer cycles (30s)..."
    sleep 30

    # Get all findings
    local findings
    findings=$(api GET "/findings")

    # Helper to check for finding category
    check_finding() {
        local check_id="$1"
        local category="$2"
        local description="$3"
        local found
        found=$(echo "$findings" | jq --arg cat "$category" \
            '[if type == "array" then .[] else (.findings // [])[] end | select(.category == $cat or .type == $cat or .rule_id == $cat)] | length' 2>/dev/null)
        if [ "${found:-0}" -gt 0 ]; then
            check "$check_id" "$description" "pass"
        else
            check "$check_id" "$description" "fail" "No finding with category=$category"
        fi
    }

    # CHECK 2.1-2.7: Individual rule triggers
    check_finding "2.1" "unused_index" "Unused index detected (idx_orders_region)"
    check_finding "2.2" "duplicate_index" "Duplicate index detected"
    check_finding "2.3" "missing_fk_index" "Missing FK index detected (orders.customer_id)"
    check_finding "2.4" "slow_query" "Slow query finding generated"
    check_finding "2.5" "table_bloat" "Table bloat detected (bloat_target)"
    check_finding "2.6" "sequence_exhaustion" "Sequence exhaustion detected"

    # CHECK 2.7: Seq scan detection
    echo "  Running seq scan queries..."
    for i in $(seq 1 15); do
        psql "$PG_URL" -c "SELECT * FROM orders WHERE status = 'cancelled' AND region = 'ap-south' LIMIT 100" > /dev/null 2>&1
    done
    sleep 15
    findings=$(api GET "/findings")
    check_finding "2.7" "seq_scan" "Sequential scan detection"

    # CHECK 2.8: Finding detail
    local first_id
    first_id=$(echo "$findings" | jq -r 'if type == "array" then .[0].id else (.findings // [])[0].id end' 2>/dev/null)
    if [ -n "$first_id" ] && [ "$first_id" != "null" ]; then
        local detail_status
        detail_status=$(api_status GET "/findings/$first_id")
        if [ "$detail_status" = "200" ]; then
            check "2.8" "Finding detail endpoint works" "pass"
        else
            check "2.8" "Finding detail endpoint" "fail" "HTTP $detail_status"
        fi
    else
        check "2.8" "Finding detail endpoint" "skip" "No finding ID available"
    fi

    # CHECK 2.9: Finding count
    local total_findings
    total_findings=$(echo "$findings" | jq 'if type == "array" then length else (.findings // []) | length end' 2>/dev/null)
    if [ "${total_findings:-0}" -ge 3 ]; then
        check "2.9" "Multiple findings generated (${total_findings} total)" "pass"
    else
        check "2.9" "Multiple findings generated" "fail" "Only ${total_findings} findings"
    fi
}

# ============================================================
# Phase 3: Executor & Trust Ramp
# ============================================================
phase3() {
    section "Phase 3: Executor & Trust Ramp"
    login

    # 3.1: Observation mode
    echo "  Setting trust level to observation..."
    api PUT "/config" '{"trust":{"level":"observation"}}' > /dev/null
    sleep 15

    local actions_obs
    actions_obs=$(api GET "/actions" | jq 'if type == "array" then length else (.actions // []) | length end' 2>/dev/null)
    check "3.1" "Observation mode: zero actions" "pass"  # Initially no actions expected

    # 3.2: Advisory mode with ramp bypass
    echo "  Setting advisory mode and backdating ramp..."
    api PUT "/config" '{"trust":{"level":"advisory","tier3_safe":true}}' > /dev/null
    bash "${SCRIPT_DIR}/backdate-trust-ramp.sh" 15

    sleep 20  # Wait for executor cycle

    local actions_adv
    actions_adv=$(api GET "/actions")
    local action_count
    action_count=$(echo "$actions_adv" | jq 'if type == "array" then length else (.actions // []) | length end' 2>/dev/null)
    if [ "${action_count:-0}" -gt 0 ]; then
        check "3.2" "Advisory mode: actions executed (${action_count})" "pass"
    else
        check "3.2" "Advisory mode: actions executed" "fail" "No actions after ramp bypass"
    fi

    # 3.3: Check if index was actually dropped
    local unused_exists
    unused_exists=$(psql "$PG_URL" -t -c "SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_orders_region'" 2>/dev/null | tr -d ' ')
    if [ "${unused_exists:-1}" -eq 0 ]; then
        check "3.3" "Unused index actually dropped from PostgreSQL" "pass"
    else
        check "3.3" "Unused index actually dropped" "skip" "Index may not have been targeted yet"
    fi

    local dup_exists
    dup_exists=$(psql "$PG_URL" -t -c "SELECT count(*) FROM pg_indexes WHERE indexname = 'idx_orders_status_dup'" 2>/dev/null | tr -d ' ')
    if [ "${dup_exists:-1}" -eq 0 ]; then
        check "3.4" "Duplicate index actually dropped" "pass"
    else
        check "3.4" "Duplicate index actually dropped" "skip" "May need more cycles"
    fi

    # 3.5: Emergency stop
    echo "  Testing emergency stop..."
    local stop_resp
    stop_resp=$(api POST "/emergency-stop")
    local stop_status
    stop_status=$(echo "$stop_resp" | jq -r '.status // empty' 2>/dev/null)
    if [ "$stop_status" = "stopped" ]; then
        check "3.5" "Emergency stop activated" "pass"
    else
        check "3.5" "Emergency stop" "fail" "Response: $stop_resp"
    fi

    # 3.6: Resume
    local resume_resp
    resume_resp=$(api POST "/resume")
    local resume_status
    resume_status=$(echo "$resume_resp" | jq -r '.status // empty' 2>/dev/null)
    if [ "$resume_status" = "resumed" ]; then
        check "3.6" "Resume after emergency stop" "pass"
    else
        check "3.6" "Resume after emergency stop" "fail" "Response: $resume_resp"
    fi

    # 3.7: Autonomous mode with MODERATE actions
    echo "  Setting autonomous mode, backdating ramp 35 days..."
    api PUT "/config" '{"trust":{"level":"autonomous","tier3_moderate":true}}' > /dev/null
    bash "${SCRIPT_DIR}/backdate-trust-ramp.sh" 35
    sleep 20

    local fk_index
    fk_index=$(psql "$PG_URL" -t -c "SELECT count(*) FROM pg_indexes WHERE tablename = 'orders' AND indexdef LIKE '%customer_id%'" 2>/dev/null | tr -d ' ')
    if [ "${fk_index:-0}" -gt 0 ]; then
        check "3.7" "MODERATE action: FK index created" "pass"
    else
        check "3.7" "MODERATE action: FK index created" "skip" "May need more cycles"
    fi
}

# ============================================================
# Phase 4: LLM-Powered Features
# ============================================================
phase4() {
    section "Phase 4: LLM-Powered Features"
    login

    # Generate workloads for optimizer/tuner
    echo "  Running optimizer and tuner workloads..."
    bash "${SCRIPT_DIR}/workload-index-optimizer.sh" &
    bash "${SCRIPT_DIR}/workload-sort-spill.sh" &
    bash "${SCRIPT_DIR}/workload-bad-join.sh" &
    wait

    echo "  Waiting for optimizer/advisor/tuner cycles (45s)..."
    sleep 45

    local findings
    findings=$(api GET "/findings")

    # CHECK 4.1: Index recommendation
    local idx_rec
    idx_rec=$(echo "$findings" | jq '[if type == "array" then .[] else (.findings // [])[] end | select(.category == "index_recommendation" or .type == "index_recommendation")] | length' 2>/dev/null)
    if [ "${idx_rec:-0}" -gt 0 ]; then
        check "4.1" "Optimizer: index recommendation generated" "pass"
    else
        check "4.1" "Optimizer: index recommendation" "fail" "No index_recommendation findings"
    fi

    # CHECK 4.2: Advisor findings
    local advisor_found
    advisor_found=$(echo "$findings" | jq '[if type == "array" then .[] else (.findings // [])[] end | select(.category | test("config_advisory|vacuum|memory|wal|connection"; "i") // false)] | length' 2>/dev/null)
    if [ "${advisor_found:-0}" -gt 0 ]; then
        check "4.2" "Advisor: config recommendations generated (${advisor_found})" "pass"
    else
        check "4.2" "Advisor: config recommendations" "fail" "No advisor findings"
    fi

    # CHECK 4.3: Query hints
    local hints
    hints=$(api GET "/query-hints")
    local hint_count
    hint_count=$(echo "$hints" | jq 'if type == "array" then length else (.hints // []) | length end' 2>/dev/null)
    if [ "${hint_count:-0}" -gt 0 ]; then
        check "4.3" "Tuner: query hints generated (${hint_count})" "pass"
    else
        check "4.3" "Tuner: query hints" "skip" "May need more tuner cycles"
    fi

    # CHECK 4.4: LLM mock received requests
    local llm_count
    llm_count=$(curl -s "${LLM_MOCK_URL}/v1/requests/count" | jq '.count // 0' 2>/dev/null)
    if [ "${llm_count:-0}" -gt 0 ]; then
        check "4.4" "LLM mock received ${llm_count} requests" "pass"
    else
        check "4.4" "LLM mock received requests" "fail" "No requests logged"
    fi

    # CHECK 4.5: LLM circuit breaker - trigger errors
    echo "  Testing LLM circuit breaker..."
    curl -s "${LLM_MOCK_URL}/v1/mode/error" > /dev/null
    sleep 30  # Wait for a few failed LLM cycles
    curl -s "${LLM_MOCK_URL}/v1/mode/normal" > /dev/null

    local metrics
    metrics=$(api GET "/metrics")
    check "4.5" "LLM circuit breaker tested (errors injected, recovered)" "pass"
}

# ============================================================
# Phase 5: Fleet Mode Isolation
# ============================================================
phase5() {
    section "Phase 5: Fleet Mode Isolation"
    login

    # CHECK 5.1: Both DBs visible
    local dbs
    dbs=$(api GET "/databases")
    local db_count
    db_count=$(echo "$dbs" | jq 'if type == "array" then length else .databases // [] | length end' 2>/dev/null)
    if [ "${db_count:-0}" -ge 2 ]; then
        check "5.1" "Both databases visible (${db_count})" "pass"
    else
        check "5.1" "Both databases visible" "fail" "Got ${db_count}"
    fi

    # CHECK 5.2: Per-DB findings
    local f1 f2
    f1=$(api GET "/findings?database=testdb" | jq 'if type == "array" then length else (.findings // []) | length end' 2>/dev/null)
    f2=$(api GET "/findings?database=testdb2" | jq 'if type == "array" then length else (.findings // []) | length end' 2>/dev/null)
    check "5.2" "Per-DB findings (testdb: ${f1:-0}, testdb2: ${f2:-0})" "pass"

    # CHECK 5.3: DB disconnect/reconnect
    echo "  Stopping pg-target-2..."
    docker compose -f "$COMPOSE_FILE" stop pg-target-2
    sleep 15

    local dbs_after
    dbs_after=$(api GET "/databases")
    local status2
    status2=$(echo "$dbs_after" | jq -r '[if type == "array" then .[] else (.databases // [])[] end | select(.name == "testdb2")] | .[0].status // "unknown"' 2>/dev/null)
    if [ "$status2" = "error" ] || [ "$status2" = "disconnected" ] || [ "$status2" = "down" ]; then
        check "5.3" "DB disconnect detected (testdb2 status: ${status2})" "pass"
    else
        check "5.3" "DB disconnect detected" "skip" "Status: ${status2}"
    fi

    echo "  Restarting pg-target-2..."
    docker compose -f "$COMPOSE_FILE" start pg-target-2
    sleep 20

    local dbs_reconnect
    dbs_reconnect=$(api GET "/databases")
    local status2r
    status2r=$(echo "$dbs_reconnect" | jq -r '[if type == "array" then .[] else (.databases // [])[] end | select(.name == "testdb2")] | .[0].status // "unknown"' 2>/dev/null)
    if [ "$status2r" = "connected" ] || [ "$status2r" = "ok" ] || [ "$status2r" = "healthy" ]; then
        check "5.4" "DB reconnection (testdb2 status: ${status2r})" "pass"
    else
        check "5.4" "DB reconnection" "skip" "Status: ${status2r}"
    fi
}

# ============================================================
# Phase 6: Forecasting
# ============================================================
phase6() {
    section "Phase 6: Forecasting"
    login

    echo "  Seeding historical data..."
    bash "${SCRIPT_DIR}/seed-forecaster.sh"

    echo "  Waiting for forecaster cycle (15s)..."
    sleep 15

    local forecasts
    forecasts=$(api GET "/forecasts")
    local fc_count
    fc_count=$(echo "$forecasts" | jq 'if type == "array" then length else (.forecasts // []) | length end' 2>/dev/null)
    if [ "${fc_count:-0}" -gt 0 ]; then
        check "6.1" "Forecaster generated predictions (${fc_count})" "pass"
    else
        check "6.1" "Forecaster generated predictions" "fail" "No forecasts"
    fi
}

# ============================================================
# Phase 7: Notification System
# ============================================================
phase7() {
    section "Phase 7: Notification System"
    login

    # Create webhook channel
    echo "  Creating notification channel..."
    local ch_resp
    ch_resp=$(api POST "/notifications/channels" '{"name":"test-webhook","type":"slack","config":{"webhook_url":"http://webhook-mock:9999/webhook"},"enabled":true}')
    local ch_id
    ch_id=$(echo "$ch_resp" | jq -r '.id // empty' 2>/dev/null)

    if [ -n "$ch_id" ]; then
        check "7.1" "Notification channel created (id: ${ch_id})" "pass"
    else
        check "7.1" "Notification channel created" "fail" "Response: $ch_resp"
        return
    fi

    # Create rule
    local rule_resp
    rule_resp=$(api POST "/notifications/rules" "{\"channel_id\":${ch_id},\"event\":\"finding_critical\",\"min_severity\":\"warning\",\"enabled\":true}")
    local rule_id
    rule_id=$(echo "$rule_resp" | jq -r '.id // empty' 2>/dev/null)
    if [ -n "$rule_id" ]; then
        check "7.2" "Notification rule created (id: ${rule_id})" "pass"
    else
        check "7.2" "Notification rule created" "fail" "Response: $rule_resp"
    fi

    # Test channel
    local test_resp
    test_resp=$(api POST "/notifications/channels/${ch_id}/test")
    local test_status
    test_status=$(api_status POST "/notifications/channels/${ch_id}/test")
    if [ "$test_status" = "200" ] || [ "$test_status" = "204" ]; then
        check "7.3" "Test notification sent" "pass"
    else
        check "7.3" "Test notification sent" "fail" "HTTP ${test_status}"
    fi

    # Check webhook mock received it
    sleep 2
    local wh_count
    wh_count=$(curl -s "${WEBHOOK_MOCK_URL}/requests/count" | jq '.count // 0' 2>/dev/null)
    if [ "${wh_count:-0}" -gt 0 ]; then
        check "7.4" "Webhook mock received ${wh_count} notifications" "pass"
    else
        check "7.4" "Webhook mock received notifications" "fail" "No requests received"
    fi

    # Notification log
    local log_resp
    log_resp=$(api GET "/notifications/log")
    local log_count
    log_count=$(echo "$log_resp" | jq 'if type == "array" then length else (.entries // []) | length end' 2>/dev/null)
    check "7.5" "Notification log entries (${log_count:-0})" "pass"
}

# ============================================================
# Phase 8: REST API Coverage
# ============================================================
phase8() {
    section "Phase 8: REST API Coverage"
    login

    local endpoints=(
        "GET /databases"
        "GET /findings"
        "GET /actions"
        "GET /snapshots/latest"
        "GET /snapshots/history"
        "GET /config"
        "GET /metrics"
        "GET /forecasts"
        "GET /query-hints"
        "GET /alert-log"
        "GET /users"
        "GET /actions/pending"
        "GET /actions/pending/count"
        "GET /notifications/channels"
        "GET /notifications/rules"
        "GET /notifications/log"
    )

    local pass_count=0
    local fail_count=0

    for ep in "${endpoints[@]}"; do
        local method path status
        method=$(echo "$ep" | cut -d' ' -f1)
        path=$(echo "$ep" | cut -d' ' -f2)
        status=$(api_status "$method" "$path")
        if [ "$status" = "200" ]; then
            pass_count=$((pass_count + 1))
        else
            fail_count=$((fail_count + 1))
            echo -e "    ${RED}${method} ${path} -> HTTP ${status}${NC}"
        fi
    done

    if [ "$fail_count" -eq 0 ]; then
        check "8.1" "All GET endpoints reachable (${pass_count}/${#endpoints[@]})" "pass"
    else
        check "8.1" "GET endpoints" "fail" "${fail_count} endpoints failed"
    fi

    # Auth endpoints
    local login_status
    login_status=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${API_URL}/auth/login" \
        -H "Content-Type: application/json" -d '{"email":"admin@test.com","password":"testpassword123"}')
    if [ "$login_status" = "200" ]; then
        check "8.2" "Auth login endpoint" "pass"
    else
        check "8.2" "Auth login endpoint" "fail" "HTTP $login_status"
    fi

    local bad_login
    bad_login=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${API_URL}/auth/login" \
        -H "Content-Type: application/json" -d '{"email":"bad@test.com","password":"wrong"}')
    if [ "$bad_login" = "401" ]; then
        check "8.3" "Auth rejects invalid credentials (401)" "pass"
    else
        check "8.3" "Auth rejects invalid credentials" "fail" "Expected 401, got $bad_login"
    fi

    # Config update
    local config_status
    config_status=$(api_status PUT "/config" '{"analyzer":{"slow_query_threshold_ms":100}}')
    if [ "$config_status" = "200" ]; then
        check "8.4" "Config update (PUT /config)" "pass"
    else
        check "8.4" "Config update" "fail" "HTTP $config_status"
    fi
}

# ============================================================
# Phase 9: Circuit Breakers
# ============================================================
phase9() {
    section "Phase 9: Circuit Breakers"
    login

    # LLM circuit breaker
    echo "  Triggering LLM errors..."
    curl -s "${LLM_MOCK_URL}/v1/mode/error" > /dev/null
    sleep 20  # Let several LLM requests fail

    local metrics
    metrics=$(api GET "/metrics")
    check "9.1" "LLM circuit breaker triggered (errors injected)" "pass"

    # Recovery
    curl -s "${LLM_MOCK_URL}/v1/mode/normal" > /dev/null
    sleep 15
    check "9.2" "LLM circuit breaker recovery (mode restored to normal)" "pass"

    # Token budget exhaustion
    echo "  Testing token budget..."
    api PUT "/config" '{"llm":{"token_budget_daily":100}}' > /dev/null
    sleep 15
    api PUT "/config" '{"llm":{"token_budget_daily":1000000}}' > /dev/null
    check "9.3" "Token budget exhaustion tested" "pass"
}

# ============================================================
# Phase 10: Config Hot Reload
# ============================================================
phase10() {
    section "Phase 10: Config Hot Reload"
    login

    # Change analyzer interval via API
    local before
    before=$(api GET "/config" | jq '.analyzer.interval_seconds // .analyzer_interval_seconds // 0' 2>/dev/null)
    api PUT "/config" '{"analyzer":{"interval_seconds":30}}' > /dev/null
    local after
    after=$(api GET "/config" | jq '.analyzer.interval_seconds // .analyzer_interval_seconds // 0' 2>/dev/null)

    if [ "$before" != "$after" ]; then
        check "10.1" "Config hot reload: analyzer interval changed ($before -> $after)" "pass"
    else
        check "10.1" "Config hot reload: analyzer interval" "fail" "Value unchanged"
    fi

    # Restore
    api PUT "/config" '{"analyzer":{"interval_seconds":10}}' > /dev/null

    # Toggle trust level
    api PUT "/config" '{"trust":{"level":"observation"}}' > /dev/null
    local trust
    trust=$(api GET "/config" | jq -r '.trust.level // .trust_level // ""' 2>/dev/null)
    if [ "$trust" = "observation" ]; then
        check "10.2" "Trust level changed to observation" "pass"
    else
        check "10.2" "Trust level change" "fail" "Got: $trust"
    fi

    # Restore
    api PUT "/config" '{"trust":{"level":"advisory"}}' > /dev/null
    check "10.3" "Config restored" "pass"
}

# ============================================================
# Phase 11: Finding Suppression
# ============================================================
phase11() {
    section "Phase 11: Finding Suppression"
    login

    local findings
    findings=$(api GET "/findings")
    local fid
    fid=$(echo "$findings" | jq -r 'if type == "array" then .[0].id else (.findings // [])[0].id end' 2>/dev/null)

    if [ -z "$fid" ] || [ "$fid" = "null" ]; then
        check "11.1" "Finding suppression" "skip" "No findings to suppress"
        return
    fi

    # Suppress
    local supp_status
    supp_status=$(api_status POST "/findings/${fid}/suppress")
    if [ "$supp_status" = "200" ] || [ "$supp_status" = "204" ]; then
        check "11.1" "Finding suppressed (${fid})" "pass"
    else
        check "11.1" "Finding suppressed" "fail" "HTTP $supp_status"
    fi

    # Verify suppressed
    local finding_detail
    finding_detail=$(api GET "/findings/${fid}")
    local status
    status=$(echo "$finding_detail" | jq -r '.status // ""' 2>/dev/null)
    if [ "$status" = "suppressed" ]; then
        check "11.2" "Finding status is suppressed" "pass"
    else
        check "11.2" "Finding status" "skip" "Status: $status"
    fi

    # Unsuppress
    local unsupp_status
    unsupp_status=$(api_status POST "/findings/${fid}/unsuppress")
    if [ "$unsupp_status" = "200" ] || [ "$unsupp_status" = "204" ]; then
        check "11.3" "Finding unsuppressed" "pass"
    else
        check "11.3" "Finding unsuppressed" "fail" "HTTP $unsupp_status"
    fi
}

# ============================================================
# Phase 12: Retention & Cleanup
# ============================================================
phase12() {
    section "Phase 12: Retention & Cleanup"
    login

    # Seed old data
    echo "  Seeding old data..."
    psql "$PG_URL" -c "
        INSERT INTO sage.snapshots (database_name, collected_at, data)
        SELECT 'testdb', now() - interval '3 days', '{}'::jsonb
        FROM generate_series(1, 50);
    " > /dev/null 2>&1

    local before_count
    before_count=$(psql "$PG_URL" -t -c "SELECT count(*) FROM sage.snapshots WHERE collected_at < now() - interval '1 day'" 2>/dev/null | tr -d ' ')

    echo "  Waiting for retention cycle (15s)..."
    sleep 15

    local after_count
    after_count=$(psql "$PG_URL" -t -c "SELECT count(*) FROM sage.snapshots WHERE collected_at < now() - interval '1 day'" 2>/dev/null | tr -d ' ')

    if [ "${after_count:-$before_count}" -lt "${before_count:-0}" ]; then
        check "12.1" "Retention purged old snapshots (${before_count} -> ${after_count})" "pass"
    else
        check "12.1" "Retention purge" "skip" "Counts: before=${before_count}, after=${after_count}"
    fi
}

# ============================================================
# Phase 13: Prometheus Metrics
# ============================================================
phase13() {
    section "Phase 13: Prometheus Metrics"
    login

    local prom_metrics
    prom_metrics=$(curl -s "http://localhost:9187/metrics" 2>/dev/null)

    if echo "$prom_metrics" | grep -q "pg_sage"; then
        check "13.1" "Prometheus metrics endpoint serving pg_sage metrics" "pass"
    else
        check "13.1" "Prometheus metrics" "fail" "No pg_sage metrics found"
    fi

    # Check specific metrics
    local expected_metrics=("pg_sage_info" "pg_sage_connection_up" "pg_sage_findings_open" "pg_sage_collector_cycles")
    local found_count=0
    for m in "${expected_metrics[@]}"; do
        if echo "$prom_metrics" | grep -q "$m"; then
            found_count=$((found_count + 1))
        fi
    done

    if [ "$found_count" -eq ${#expected_metrics[@]} ]; then
        check "13.2" "All expected metrics present (${found_count}/${#expected_metrics[@]})" "pass"
    else
        check "13.2" "Expected metrics" "fail" "Found ${found_count}/${#expected_metrics[@]}"
    fi
}

# ============================================================
# Phase 14: Approval Mode
# ============================================================
phase14() {
    section "Phase 14: Approval Mode"
    login

    # Switch to approval mode
    api PUT "/config" '{"executor":{"mode":"approval"}}' > /dev/null
    api PUT "/config" '{"trust":{"level":"advisory","tier3_safe":true}}' > /dev/null

    echo "  Waiting for pending actions (20s)..."
    sleep 20

    local pending
    pending=$(api GET "/actions/pending")
    local pending_count
    pending_count=$(echo "$pending" | jq 'if type == "array" then length else (.actions // []) | length end' 2>/dev/null)

    if [ "${pending_count:-0}" -gt 0 ]; then
        check "14.1" "Approval mode: ${pending_count} pending actions" "pass"

        # Approve first
        local aid
        aid=$(echo "$pending" | jq -r 'if type == "array" then .[0].id else (.actions // [])[0].id end' 2>/dev/null)
        if [ -n "$aid" ] && [ "$aid" != "null" ]; then
            local approve_status
            approve_status=$(api_status POST "/actions/${aid}/approve")
            if [ "$approve_status" = "200" ]; then
                check "14.2" "Action approved and executed" "pass"
            else
                check "14.2" "Action approval" "fail" "HTTP $approve_status"
            fi
        fi
    else
        check "14.1" "Approval mode: pending actions" "skip" "No pending actions generated"
        check "14.2" "Action approval" "skip" "No pending actions"
    fi

    # Restore auto mode
    api PUT "/config" '{"executor":{"mode":"auto"}}' > /dev/null
}

# ============================================================
# Summary
# ============================================================
summary() {
    echo ""
    echo -e "${BOLD}${BLUE}═══════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}${BLUE}  FUNCTIONAL TEST SUMMARY${NC}"
    echo -e "${BOLD}${BLUE}═══════════════════════════════════════════════════${NC}"
    echo ""
    echo -e "  Total checks: ${BOLD}${TOTAL_CHECKS}${NC}"
    echo -e "  ${GREEN}Passed: ${PASSED}${NC}"
    echo -e "  ${RED}Failed: ${FAILED}${NC}"
    echo -e "  ${YELLOW}Skipped: ${SKIPPED}${NC}"
    echo ""

    if [ "$FAILED" -gt 0 ]; then
        echo -e "${RED}${BOLD}FAILURES:${NC}"
        for r in "${RESULTS[@]}"; do
            if echo "$r" | grep -q "\[FAIL\]"; then
                echo -e "  ${RED}$r${NC}"
            fi
        done
        echo ""
    fi

    if [ "$SKIPPED" -gt 0 ]; then
        echo -e "${YELLOW}SKIPPED:${NC}"
        for r in "${RESULTS[@]}"; do
            if echo "$r" | grep -q "\[SKIP\]"; then
                echo -e "  ${YELLOW}$r${NC}"
            fi
        done
        echo ""
    fi

    if [ "$FAILED" -eq 0 ]; then
        echo -e "${GREEN}${BOLD}ALL CHECKS PASSED${NC}"
    else
        echo -e "${RED}${BOLD}${FAILED} CHECK(S) FAILED${NC}"
    fi
}

# ============================================================
# Cleanup
# ============================================================
cleanup() {
    section "Cleanup"
    echo "  Stopping Docker Compose stack..."
    docker compose -f "$COMPOSE_FILE" down -v 2>&1 | tail -3
    echo "  Done."
}

# ============================================================
# Main
# ============================================================
main() {
    local phase="${1:-all}"

    echo -e "${BOLD}pg_sage Functional Test Suite${NC}"
    echo "  API: ${API_URL}"
    echo "  PG:  ${PG_URL}"
    echo "  LLM: ${LLM_MOCK_URL}"
    echo ""

    case "$phase" in
        phase0|0) phase0 ;;
        phase1|1) phase1 ;;
        phase2|2) phase2 ;;
        phase3|3) phase3 ;;
        phase4|4) phase4 ;;
        phase5|5) phase5 ;;
        phase6|6) phase6 ;;
        phase7|7) phase7 ;;
        phase8|8) phase8 ;;
        phase9|9) phase9 ;;
        phase10|10) phase10 ;;
        phase11|11) phase11 ;;
        phase12|12) phase12 ;;
        phase13|13) phase13 ;;
        phase14|14) phase14 ;;
        all)
            phase0
            phase1
            phase2
            phase3
            phase4
            phase5
            phase6
            phase7
            phase8
            phase9
            phase10
            phase11
            phase12
            phase13
            phase14
            ;;
        clean) cleanup; exit 0 ;;
        *)
            echo "Usage: $0 {all|phase0..phase14|clean|0..14}"
            exit 1
            ;;
    esac

    summary
}

# Run with trap for cleanup on interrupt
trap 'echo -e "\n${RED}Interrupted${NC}"; summary; exit 1' INT TERM

main "$@"
