#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== pg_sage v0.8.0 Live Integration Test ==="

# Check for Gemini API key
if [ -z "$SAGE_GEMINI_API_KEY" ]; then
    echo "ERROR: SAGE_GEMINI_API_KEY not set"
    echo "Usage: SAGE_GEMINI_API_KEY=your-key ./demo/run-live.sh"
    exit 1
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
go build -o pg_sage_sidecar ./cmd/pg_sage_sidecar/

echo ""
echo "=== Demo environment ready ==="
echo "PostgreSQL: localhost:5433 (sage_demo / sage_agent / sage_password)"
echo ""
echo "Starting sidecar..."
echo "Dashboard: http://localhost:8080"
echo "API:       http://localhost:8080/api/v1/databases"
echo "MCP:       http://localhost:5434/health"
echo "Metrics:   http://localhost:9187/metrics"
echo ""

# Run sidecar
./pg_sage_sidecar --config "$SCRIPT_DIR/config-live.yaml"
