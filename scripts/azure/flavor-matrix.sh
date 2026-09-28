#!/usr/bin/env bash
# Run the Azure live checklist against every PostgreSQL flavor Azure can
# still create, one at a time: provision, check, delete. Sequential runs
# stay inside free-trial vCore quotas and keep each server's life to
# ~20-40 minutes (a few US dollars for the whole matrix).
#
# Flavors (select with FLAVORS="flex-16 cosmos ..."):
#   flex-<v>     flexible server, Burstable B1ms, every version the region offers
#   flex-gp      flexible server, General Purpose D2ds_v5, newest version
#   elastic      elastic cluster (Citus on flexible server), 2 nodes
#   cosmos       Azure Cosmos DB for PostgreSQL, burstable single node
# Not creatable: single server (retired 2025), HorizonDB (gated preview).
#
# Results: reviews/azure-matrix-<date>.md (no credentials).
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOCATION="${AZ_LOCATION:-centralus}"
export AZ_LOCATION="$LOCATION" AZ_RESOURCE_GROUP="${AZ_RESOURCE_GROUP:-pg-sage-test}"
STATE_DIR="$HOME/.pg_sage/azure-matrix"
REPORT="${MATRIX_REPORT:-$REPO/reviews/azure-matrix-$(date +%Y-%m-%d).md}"
mkdir -p "$STATE_DIR"

az account show >/dev/null 2>&1 || { echo "run 'az login' first" >&2; exit 1; }
az account show --query id -o tsv | grep -q . || { echo "no subscription" >&2; exit 1; }

versions() {
	az postgres flexible-server list-skus -l "$LOCATION" \
		--query "[0].supportedServerVersions[].name" -o tsv 2>/dev/null | sort -n |
		grep -E '^[0-9]+$' || printf '%s\n' 13 14 15 16 17
}

if [[ -z "${FLAVORS:-}" ]]; then
	FLAVORS="$(versions | sed 's/^/flex-/' | tr '\n' ' ')flex-gp elastic cosmos"
fi
newest="$(versions | tail -1)"

provision() { # flavor -> runs the right provisioner with flavor settings
	case "$1" in
	flex-gp) AZ_PG_VERSION="$newest" AZ_TIER=GeneralPurpose AZ_SKU=Standard_D2ds_v5 \
		"$REPO/scripts/azure/provision-test-server.sh" ;;
	elastic) AZ_PG_VERSION="$newest" AZ_TIER=GeneralPurpose AZ_SKU=Standard_D2ds_v5 \
		AZ_NODE_COUNT=2 "$REPO/scripts/azure/provision-test-server.sh" ;;
	cosmos) "$REPO/scripts/azure/provision-cosmos-cluster.sh" ;;
	flex-*) AZ_PG_VERSION="${1#flex-}" "$REPO/scripts/azure/provision-test-server.sh" ;;
	*) echo "unknown flavor $1" >&2; return 1 ;;
	esac
}

delete_target() { # flavor env file already sourced
	if [[ "${SAGE_AZURE_KIND:-flexible}" == cosmos ]]; then
		az cosmosdb postgres cluster delete -g "$SAGE_AZURE_RESOURCE_GROUP" \
			-n "$SAGE_AZURE_SERVER_NAME" --yes -o none
	else
		az postgres flexible-server delete -g "$SAGE_AZURE_RESOURCE_GROUP" \
			-n "$SAGE_AZURE_SERVER_NAME" --yes -o none
	fi
}

run_flavor() {
	local flavor="$1" env="$STATE_DIR/$1.env" log="$STATE_DIR/$1.log" status=PASS
	export SAGE_AZURE_ENV_FILE="$env"
	echo "== $flavor" >&2
	if ! provision "$flavor" >"$log" 2>&1; then
		status="PROVISION FAILED"
	elif ! "$REPO/scripts/azure/live-checklist.sh" >>"$log" 2>&1; then
		status=FAIL
	fi
	{
		echo "## $flavor: $status"
		echo
		echo '```'
		grep -E "^CHECK-AZ|ERROR|refused|permission denied|does not exist|not allow" "$log" |
			grep -v -i password
		echo '```'
		echo
	} >>"$REPORT"
	if [[ -f "$env" ]]; then
		# shellcheck disable=SC1090
		source "$env"
		[[ "${KEEP:-0}" == 1 ]] || { delete_target && rm -f "$env"; }
		unset SAGE_AZURE_KIND SAGE_AZURE_DB SAGE_AZURE_SERVER_NAME SAGE_AZURE_ADMIN_PASSWORD \
			SAGE_AZURE_AGENT_PASSWORD
	fi
	echo "== $flavor: $status" >&2
}

{
	echo "# Azure flavor matrix $(date +%Y-%m-%d)"
	echo
	echo "Region $LOCATION. Flavors: $FLAVORS"
	echo
} >"$REPORT"
for flavor in $FLAVORS; do
	run_flavor "$flavor"
done
echo "report: $REPORT (per-flavor logs in $STATE_DIR)" >&2
