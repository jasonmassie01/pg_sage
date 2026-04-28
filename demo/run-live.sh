#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== pg_sage v0.9 Live Demo ==="

if [ -z "$SAGE_GEMINI_API_KEY" ]; then
    echo "SAGE_GEMINI_API_KEY not set; LLM features remain disabled unless you add a key."
fi

# Stop any existing demo
docker compose -f "$SCRIPT_DIR/docker-compose-live.yml" down -v 2>/dev/null || true

# Start PostgreSQL
echo "Starting PostgreSQL 17 on port 5433..."
docker compose -f "$SCRIPT_DIR/docker-compose-live.yml" up -d

echo "Waiting for PostgreSQL to be ready..."
until docker compose -f "$SCRIPT_DIR/docker-compose-live.yml" exec -T postgres pg_isready -U postgres 2>/dev/null; do
    sleep 1
done
echo "PostgreSQL ready."

# Build sidecar
echo "Building sidecar..."
cd "$PROJECT_DIR/sidecar"
cd web && npm ci && npm run build && cd ..
go build -o pg_sage_sidecar ./cmd/pg_sage_sidecar/

echo ""
echo "=== Demo environment ready ==="
echo "PostgreSQL: localhost:5433 (sage_demo / sage_agent / sage_password)"
echo ""
echo "Starting sidecar..."
echo "Dashboard: http://localhost:8080"
echo "Login:     admin@pg-sage.local (copy the INITIAL ADMIN PASSWORD from stderr)"
echo "API:       http://localhost:8080/api/v1/cases (requires login cookie)"
echo "Metrics:   http://localhost:9187/metrics"
echo ""

# Run sidecar
./pg_sage_sidecar --config "$SCRIPT_DIR/config-live.yaml"
