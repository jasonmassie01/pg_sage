#!/usr/bin/env bash
# =============================================================================
#  pg_sage Interactive Demo Script
#
#  Records well with asciinema:
#    asciinema rec -c "./demo.sh" pg_sage_demo.cast
#
#  Converts to GIF:
#    agg pg_sage_demo.cast pg_sage_demo.gif
# =============================================================================

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CONTAINER="${PG_SAGE_CONTAINER:-pg_sage-pg_sage-1}"
SIDECAR_HOST="${SIDECAR_HOST:-localhost}"
MCP_PORT="${MCP_PORT:-5433}"
PROM_PORT="${PROM_PORT:-9187}"
TYPING_DELAY="${TYPING_DELAY:-0.03}"       # seconds between characters
LINE_PAUSE="${LINE_PAUSE:-1.5}"            # pause after a typed command
SECTION_PAUSE="${SECTION_PAUSE:-2}"        # pause between sections

# ---------------------------------------------------------------------------
# Colors and helpers
# ---------------------------------------------------------------------------
BOLD="\033[1m"
DIM="\033[2m"
GREEN="\033[32m"
CYAN="\033[36m"
YELLOW="\033[33m"
RED="\033[31m"
RESET="\033[0m"

banner() {
    echo ""
    echo -e "${BOLD}${CYAN}# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    echo -e "${BOLD}${CYAN}#  $1${RESET}"
    echo -e "${BOLD}${CYAN}# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${RESET}"
    echo ""
    sleep "$SECTION_PAUSE"
}

comment() {
    echo -e "${DIM}-- $1${RESET}"
    sleep 0.5
}

type_cmd() {
    local cmd="$1"
    # Print prompt
    echo -ne "${GREEN}postgres=# ${RESET}"
    # Type each character with a delay
    for (( i=0; i<${#cmd}; i++ )); do
        echo -n "${cmd:$i:1}"
        sleep "$TYPING_DELAY"
    done
    echo ""
    sleep "$LINE_PAUSE"
}

run_sql() {
    local sql="$1"
    type_cmd "$sql"
    docker exec -i "$CONTAINER" psql -U postgres -x -c "$sql" 2>&1 || true
    echo ""
    sleep "$LINE_PAUSE"
}

run_sql_table() {
    local sql="$1"
    type_cmd "$sql"
    docker exec -i "$CONTAINER" psql -U postgres --pset=format=aligned -c "$sql" 2>&1 || true
    echo ""
    sleep "$LINE_PAUSE"
}

run_shell() {
    local cmd="$1"
    echo -e "${YELLOW}\$ ${RESET}${cmd}"
    sleep "$LINE_PAUSE"
    eval "$cmd" 2>&1 || true
    echo ""
    sleep "$LINE_PAUSE"
}

progress_bar() {
    local duration=$1
    local msg="${2:-Waiting}"
    local elapsed=0
    local width=40

    echo -ne "\n"
    while [ $elapsed -le $duration ]; do
        local pct=$(( elapsed * 100 / duration ))
        local filled=$(( elapsed * width / duration ))
        local empty=$(( width - filled ))

        printf "\r  ${DIM}%s [${RESET}" "$msg"
        printf "%0.s${GREEN}#${RESET}" $(seq 1 $filled 2>/dev/null) || true
        printf "%0.s${DIM}-${RESET}" $(seq 1 $empty 2>/dev/null) || true
        printf "${DIM}] %3d%%${RESET}" "$pct"

        sleep 1
        elapsed=$(( elapsed + 1 ))
    done
    printf "\r  ${DIM}%s [${RESET}" "$msg"
    printf "%0.s${GREEN}#${RESET}" $(seq 1 $width)
    printf "${DIM}] 100%%${RESET}\n\n"
}

# =============================================================================
#  START OF DEMO
# =============================================================================

clear
echo ""
echo -e "${BOLD}${GREEN}"
echo "    ┌──────────────────────────────────────────────────────────┐"
echo "    │                                                          │"
echo "    │     pg_sage  —  Agentic Postgres DBA                     │"
echo "    │                                                          │"
echo "    │     Native C extension  ·  3 background workers          │"
echo "    │     Rules engine + LLM  ·  Trust-ramped actions          │"
echo "    │     MCP sidecar + Prometheus metrics                     │"
echo "    │                                                          │"
echo "    └──────────────────────────────────────────────────────────┘"
echo -e "${RESET}"
sleep 3

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "1. Starting pg_sage with Docker Compose"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Start the pg_sage container (PostgreSQL + extension + MCP sidecar)"

# Check if already running
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
    echo -e "${DIM}  Container already running, reusing existing instance.${RESET}"
else
    run_shell "docker compose up -d"
fi

comment "Wait for PostgreSQL to become healthy..."
echo -ne "  "
retries=0
until docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1; do
    echo -ne "."
    sleep 2
    retries=$((retries + 1))
    if [ $retries -gt 30 ]; then
        echo -e "\n${RED}ERROR: PostgreSQL did not become ready in time.${RESET}"
        exit 1
    fi
done
echo -e " ${GREEN}Ready!${RESET}\n"
sleep 1

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "2. Extension Status"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Verify pg_sage is loaded and check system status"
run_sql "SELECT sage.status();"

comment "The status shows version, trust level, circuit breaker state,"
comment "and whether the collector + analyzer background workers are running."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "3. Waiting for Findings"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "pg_sage's background workers collect snapshots every 30s"
comment "and run the analyzer every 60s. Waiting for findings..."

progress_bar 60 "Analyzer running"

comment "Let's see what the rules engine found."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "4. Findings — What pg_sage Detected"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "List all open findings sorted by severity"
run_sql_table "SELECT category, severity, title FROM sage.findings WHERE status = 'open' ORDER BY severity, category;"

comment "pg_sage detected duplicate indexes, sequence exhaustion,"
comment "configuration issues, security gaps, and more — all automatically."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "5. Health Briefing"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Generate a health briefing — works with or without an LLM"
run_sql "SELECT sage.briefing();"

comment "Without an LLM configured, briefings use structured Tier 1 analysis."
comment "With an LLM, briefings are enriched with natural-language summaries."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "6. JSON API Functions — Schema Analysis"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Inspect the schema of a table (used by MCP sidecar too)"
run_sql "SELECT sage.schema_json('public.orders');"

comment "schema_json() returns DDL, columns, indexes, constraints, and FKs."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "7. Slow Queries"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Show the slowest queries from pg_stat_statements"
run_sql "SELECT sage.slow_queries_json();"

comment "Surfaces top queries by mean execution time with call counts."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "8. Emergency Controls"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Emergency stop: halt all autonomous activity immediately"
run_sql "SELECT sage.emergency_stop();"

comment "Verify stopped state"
run_sql "SELECT sage.status()->>'emergency_stopped' AS stopped, sage.status()->>'circuit_state' AS circuit;"

comment "Resume normal operation"
run_sql "SELECT sage.resume();"

comment "Verify resumed"
run_sql "SELECT sage.status()->>'emergency_stopped' AS stopped, sage.status()->>'circuit_state' AS circuit;"

comment "Emergency controls trip both circuit breakers instantly."
comment "Resume resets them so analysis continues."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "9. Finding Suppression"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Suppress a known finding for 7 days"
run_sql "SELECT sage.suppress(1, 'Demo suppression', 7);"

comment "Verify suppressed status"
run_sql_table "SELECT id, category, status, suppressed_until FROM sage.findings WHERE id = 1;"

comment "Suppressed findings won't appear in reports until the period expires."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "10. MCP Sidecar — AI Assistant Protocol"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "The MCP sidecar exposes pg_sage over HTTP+SSE for AI assistants."
comment "Connect via Server-Sent Events to get a session:"

echo -e "${YELLOW}\$ ${RESET}curl -sN http://${SIDECAR_HOST}:${MCP_PORT}/sse &"
sleep "$LINE_PAUSE"

# Start SSE in background, capture output
SSE_TMPFILE=$(mktemp)
curl -sN "http://${SIDECAR_HOST}:${MCP_PORT}/sse" > "$SSE_TMPFILE" 2>/dev/null &
SSE_PID=$!
sleep 2

if [ -s "$SSE_TMPFILE" ]; then
    echo -e "${DIM}  SSE response:${RESET}"
    head -5 "$SSE_TMPFILE" | sed 's/^/  /'
    echo ""

    # Extract session ID
    SESSION_ID=$(grep -oP 'sessionId=\K[a-f0-9-]+' "$SSE_TMPFILE" || echo "")

    if [ -n "$SESSION_ID" ]; then
        comment "Session established: $SESSION_ID"
        comment "Now send an MCP initialize request:"

        echo -e "${YELLOW}\$ ${RESET}curl -s -X POST 'http://${SIDECAR_HOST}:${MCP_PORT}/messages?sessionId=${SESSION_ID}' \\"
        echo "    -H 'Content-Type: application/json' \\"
        echo '    -d '"'"'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"demo","version":"1.0"}}}'"'"
        sleep "$LINE_PAUSE"

        INIT_RESP=$(curl -s -X POST "http://${SIDECAR_HOST}:${MCP_PORT}/messages?sessionId=${SESSION_ID}" \
            -H 'Content-Type: application/json' \
            -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"demo","version":"1.0"}}}' 2>&1 || true)

        sleep 2
        # Show the SSE response
        if [ -s "$SSE_TMPFILE" ]; then
            echo -e "\n${DIM}  MCP response (via SSE):${RESET}"
            tail -5 "$SSE_TMPFILE" | sed 's/^/  /'
            echo ""
        fi
    fi
else
    echo -e "${YELLOW}  Sidecar not responding — it may not be running.${RESET}"
    echo -e "${DIM}  Start with: docker compose up${RESET}"
fi

# Clean up SSE connection
kill $SSE_PID 2>/dev/null || true
rm -f "$SSE_TMPFILE"
echo ""

comment "MCP exposes resources (sage://health, sage://findings, sage://schema/{table})"
comment "and tools (diagnose, briefing, suggest_index, review_migration)."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "11. Prometheus Metrics"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

comment "Scrape Prometheus metrics from the sidecar"

echo -e "${YELLOW}\$ ${RESET}curl -s http://${SIDECAR_HOST}:${PROM_PORT}/metrics"
sleep "$LINE_PAUSE"

METRICS=$(curl -s "http://${SIDECAR_HOST}:${PROM_PORT}/metrics" 2>&1 || true)
if [ -n "$METRICS" ] && echo "$METRICS" | grep -q "pg_sage"; then
    echo "$METRICS" | head -30
    echo -e "${DIM}  ... (truncated)${RESET}"
else
    echo -e "${YELLOW}  Prometheus endpoint not available.${RESET}"
    echo -e "${DIM}  Ensure the sidecar is running on port ${PROM_PORT}.${RESET}"
fi
echo ""

comment "Metrics include findings by severity, circuit breaker state,"
comment "extension version, and status gauges."

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
banner "12. Summary"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

echo -e "${BOLD}${GREEN}"
echo "    What we demonstrated:"
echo ""
echo "    [1]  Extension loading + system status"
echo "    [2]  Tier 1 rules engine — automatic finding detection:"
echo "           - Duplicate indexes"
echo "           - Sequence exhaustion"
echo "           - Configuration audit"
echo "           - Security (missing RLS)"
echo "           - Unused indexes"
echo "    [3]  Health briefing (Tier 1 + optional Tier 2 LLM)"
echo "    [4]  JSON API: schema_json(), slow_queries_json()"
echo "    [5]  Emergency stop / resume (circuit breaker)"
echo "    [6]  Finding suppression"
echo "    [7]  MCP sidecar (SSE + JSON-RPC for AI assistants)"
echo "    [8]  Prometheus metrics endpoint"
echo ""
echo "    pg_sage runs as background workers inside PostgreSQL."
echo "    No external agents. No SaaS. Fully self-hosted."
echo -e "${RESET}"

echo -e "${BOLD}${CYAN}"
echo "    Learn more: https://github.com/jasonmassie01/pg_sage"
echo -e "${RESET}"
echo ""
