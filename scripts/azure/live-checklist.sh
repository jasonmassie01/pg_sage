#!/usr/bin/env bash
# Run the automatable part of the Azure live checklist (docs/azure.md)
# against the server from provision-test-server.sh. Prints one
# CHECK-AZ-NN line per check; CHECK-AZ-02 is reported by the provisioning
# run, CHECK-AZ-04/05 need the dashboard and are listed as MANUAL.
#
# Changes made: work_mem and max_locks_per_transaction are set and restored
# through ARM; the sidecar creates its sage schema objects and runs for
# ~150 s at trust level observation (it changes nothing itself).
set -uo pipefail

ENV_FILE="${SAGE_AZURE_ENV_FILE:-$HOME/.pg_sage/azure-test.env}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
[[ -f "$ENV_FILE" ]] || { echo "run provision-test-server.sh first" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"

failures=0
check() { # id, pass(0/1), text
	if [[ "$2" == 0 ]]; then echo "$1: PASS $3"; else echo "$1: FAIL $3"; failures=$((failures + 1)); fi
}

cd "$REPO/sidecar" || exit 1
go build -o "$WORK/pg_sage" ./cmd/pg_sage_sidecar/ || exit 1
cat >"$WORK/config.yaml" <<EOF
prometheus:
  listen_addr: "127.0.0.1:19187"
api:
  listen_addr: "127.0.0.1:18080"
trust:
  level: observation
EOF

echo "starting the sidecar for 150 s (observation only)" >&2
# Keep only the lines the checks need: the log also carries the one-time
# admin password, which must not be written anywhere.
SAGE_TRUST_LEVEL=observation timeout 150 "$WORK/pg_sage" --config "$WORK/config.yaml" 2>&1 |
	grep --line-buffered -E "cloud environment|azure server parameters|ERROR|error" |
	grep -v -i "password" >"$WORK/sidecar.log"
sed 's/^/  sidecar| /' "$WORK/sidecar.log" >&2

grep -q "cloud environment: azure" "$WORK/sidecar.log"
check CHECK-AZ-01 $? 'sidecar logs "cloud environment: azure"'

snapshots="$(PGPASSWORD="$SAGE_AZURE_AGENT_PASSWORD" docker run --rm -e PGPASSWORD \
	postgres:17-alpine psql -tA \
	"host=$SAGE_AZURE_HOST user=sage_agent dbname=postgres sslmode=require" \
	-c "SELECT count(*) FROM sage.snapshots" 2>/dev/null || echo 0)"
rc=1; [[ "${snapshots:-0}" -gt 0 ]] && rc=0
check CHECK-AZ-03 "$rc""collector snapshots in sage.snapshots: ${snapshots:-0}"

grep -q "azure server parameters apply through ARM" "$WORK/sidecar.log"
check CHECK-AZ-06 $? "startup reports ARM parameter application"

live() { PG_SAGE_LIVE_AZURE=1 go test -count=1 -v -run "$1" ./internal/azure 2>&1; }
out="$(live '^TestAzureLiveServerParameter$')"
echo "$out" | grep -q -- "--- PASS" && ! echo "$out" | grep -q -- "--- SKIP"
check CHECK-AZ-07 $? "work_mem set and restored through ARM"
echo "$out" | grep -E "azure parameter|FAIL|Error" | sed 's/^/  /' >&2

out="$(live '^TestAzureLiveRestartBoundParameter$')"
echo "$out" | grep -q -- "--- PASS" && ! echo "$out" | grep -q -- "--- SKIP"
check CHECK-AZ-08 $? "restart-bound parameter reported pending restart"
echo "$out" | grep -E "azure parameter|FAIL|Error" | sed 's/^/  /' >&2

echo "CHECK-AZ-02: see the provisioning output (refused statements are listed there)"
echo "CHECK-AZ-04: MANUAL fleet readiness shows provider azure, log access azure_monitor"
echo "CHECK-AZ-05: MANUAL an approved ANALYZE executes and verifies"
exit "$failures"
