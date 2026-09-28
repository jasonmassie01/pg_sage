#!/usr/bin/env bash
# Provision a single-node Azure Cosmos DB for PostgreSQL (Citus) cluster for
# the pg_sage live checklist. Microsoft no longer recommends this service for
# new projects (elastic clusters on flexible server replace it), but existing
# fleets still run it. Burstable coordinator, no workers.
#
# Same contract as provision-test-server.sh: credentials are generated here
# and written only to $SAGE_AZURE_ENV_FILE (mode 600), never printed.
set -euo pipefail

ENV_FILE="${SAGE_AZURE_ENV_FILE:-$HOME/.pg_sage/azure-cosmos.env}"
LOCATION="${AZ_LOCATION:-centralus}"
RESOURCE_GROUP="${AZ_RESOURCE_GROUP:-pg-sage-test}"
PG_VERSION="${AZ_PG_VERSION:-16}"

log() { printf '[cosmos-setup] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

az account show >/dev/null 2>&1 || die "not signed in; run 'az login' first"
mkdir -p "$(dirname "$ENV_FILE")"
if [[ -f "$ENV_FILE" ]]; then
	# shellcheck disable=SC1090
	source "$ENV_FILE"
fi
CLUSTER="${SAGE_AZURE_SERVER_NAME:-pgsage-cosmos-$(openssl rand -hex 3)}"
ADMIN_PASSWORD="${SAGE_AZURE_ADMIN_PASSWORD:-$(openssl rand -hex 16)Aa9}"
AGENT_PASSWORD="${SAGE_AZURE_AGENT_PASSWORD:-$(openssl rand -hex 16)Bb7}"

write_env() { # host may be empty until the cluster exists
	umask 077
	cat >"$ENV_FILE" <<EOF
# pg_sage Azure Cosmos DB for PostgreSQL target. Contains credentials.
export SAGE_AZURE_KIND='cosmos'
export SAGE_AZURE_RESOURCE_GROUP='$RESOURCE_GROUP'
export SAGE_AZURE_SERVER_NAME='$CLUSTER'
export SAGE_AZURE_ADMIN_USER='citus'
export SAGE_AZURE_ADMIN_PASSWORD='$ADMIN_PASSWORD'
export SAGE_AZURE_AGENT_PASSWORD='$AGENT_PASSWORD'
export SAGE_AZURE_DB='citus'
export SAGE_AZURE_HOST='$1'
export SAGE_DATABASE_URL='postgres://sage_agent:$AGENT_PASSWORD@$1:5432/citus?sslmode=require'
EOF
	chmod 600 "$ENV_FILE"
}
write_env ""

az group create -n "$RESOURCE_GROUP" -l "$LOCATION" -o none
if ! az cosmosdb postgres cluster show -g "$RESOURCE_GROUP" -n "$CLUSTER" -o none 2>/dev/null; then
	log "creating cluster $CLUSTER (PostgreSQL $PG_VERSION, burstable single node; ~10-20 min)"
	az cosmosdb postgres cluster create -g "$RESOURCE_GROUP" -n "$CLUSTER" -l "$LOCATION" \
		--login-password "$ADMIN_PASSWORD" --postgresql-version "$PG_VERSION" \
		--node-count 0 --enable-shards-on-coord true \
		--coordinator-server-edition BurstableMemoryOptimized --coordinator-v-cores 1 \
		--coordinator-storage 32768 --coord-public-ip-access true -o none
fi
CLIENT_IP="$(curl -fsS https://api.ipify.org)"
az cosmosdb postgres firewall-rule create -g "$RESOURCE_GROUP" --cluster-name "$CLUSTER" \
	--firewall-rule-name pgsage-client --start-ip-address "$CLIENT_IP" \
	--end-ip-address "$CLIENT_IP" -o none
HOST="$(az cosmosdb postgres cluster show -g "$RESOURCE_GROUP" -n "$CLUSTER" \
	--query 'serverNames[0].fullyQualifiedDomainName' -o tsv)"
[[ -n "$HOST" ]] || die "cluster has no coordinator host name"
write_env "$HOST"

log "running the documented setup SQL as citus (refused statements are findings)"
PGPASSWORD="$ADMIN_PASSWORD" AGENT_PASSWORD="$AGENT_PASSWORD" docker run --rm -i \
	-e PGPASSWORD -e AGENT_PASSWORD postgres:17-alpine sh -c \
	"psql -v ON_ERROR_STOP=0 -v agent_password=\"\$AGENT_PASSWORD\" \
	'host=$HOST user=citus dbname=citus sslmode=require'" <<'SQL'
SELECT format('CREATE USER sage_agent WITH PASSWORD %L', :'agent_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sage_agent') \gexec
SELECT format('ALTER USER sage_agent WITH PASSWORD %L', :'agent_password') \gexec
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;
GRANT pg_signal_backend TO sage_agent;
GRANT sage_agent TO citus;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS hypopg;
CREATE EXTENSION IF NOT EXISTS pg_hint_plan;
CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;
SQL
log "done. Target recorded in $ENV_FILE"
