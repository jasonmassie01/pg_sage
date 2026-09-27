#!/usr/bin/env bash
# Provision a small Azure Database for PostgreSQL flexible server for the
# pg_sage live checklist (docs/azure.md, CHECK-AZ-01..08).
#
# Prerequisites: Azure CLI signed in (`az login`), Docker (for psql).
# Cost: Burstable B1ms + 32 GB storage, roughly USD 15-20/month while it
# exists. Remove everything with scripts/azure/teardown-test-server.sh.
#
# Idempotent: re-running reuses the server recorded in the env file.
# Credentials are generated here and written only to $SAGE_AZURE_ENV_FILE
# (default ~/.pg_sage/azure-test.env, mode 600); they are never printed.
set -euo pipefail

ENV_FILE="${SAGE_AZURE_ENV_FILE:-$HOME/.pg_sage/azure-test.env}"
LOCATION="${AZ_LOCATION:-centralus}"
RESOURCE_GROUP="${AZ_RESOURCE_GROUP:-pg-sage-test}"
PG_VERSION="${AZ_PG_VERSION:-17}"
ADMIN_USER="sageadmin"

log() { printf '[azure-setup] %s\n' "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

command -v az >/dev/null || die "Azure CLI not found; install it and run 'az login'"
command -v docker >/dev/null || die "Docker is required to run psql"
az account show >/dev/null 2>&1 || die "not signed in; run 'az login' first"

mkdir -p "$(dirname "$ENV_FILE")"
if [[ -f "$ENV_FILE" ]]; then
	# shellcheck disable=SC1090
	source "$ENV_FILE"
	log "reusing server ${SAGE_AZURE_SERVER_NAME} from $ENV_FILE"
fi

SUBSCRIPTION_ID="$(az account show --query id -o tsv)"
SERVER_NAME="${SAGE_AZURE_SERVER_NAME:-pgsage-test-$(openssl rand -hex 3)}"
ADMIN_PASSWORD="${SAGE_AZURE_ADMIN_PASSWORD:-$(openssl rand -hex 16)Aa9}"
AGENT_PASSWORD="${SAGE_AZURE_AGENT_PASSWORD:-$(openssl rand -hex 16)Bb7}"
HOST="${SERVER_NAME}.postgres.database.azure.com"

write_env() {
	umask 077
	cat >"$ENV_FILE" <<EOF
# pg_sage Azure live-test target. Contains credentials: keep private.
export SAGE_AZURE_SUBSCRIPTION_ID='$SUBSCRIPTION_ID'
export SAGE_AZURE_RESOURCE_GROUP='$RESOURCE_GROUP'
export SAGE_AZURE_SERVER_NAME='$SERVER_NAME'
export SAGE_AZURE_ADMIN_PASSWORD='$ADMIN_PASSWORD'
export SAGE_AZURE_AGENT_PASSWORD='$AGENT_PASSWORD'
export SAGE_AZURE_HOST='$HOST'
export SAGE_DATABASE_URL='postgres://sage_agent:$AGENT_PASSWORD@$HOST:5432/postgres?sslmode=require'
EOF
	chmod 600 "$ENV_FILE"
}
# Record names and credentials before creating anything, so a failed run
# can be resumed or torn down.
write_env

log "resource group $RESOURCE_GROUP in $LOCATION"
az group create -n "$RESOURCE_GROUP" -l "$LOCATION" -o none

if az postgres flexible-server show -g "$RESOURCE_GROUP" -n "$SERVER_NAME" -o none 2>/dev/null; then
	log "server $SERVER_NAME exists"
else
	CLIENT_IP="$(curl -fsS https://api.ipify.org)"
	log "creating server $SERVER_NAME (PostgreSQL $PG_VERSION, B1ms; ~5-10 min)"
	az postgres flexible-server create -g "$RESOURCE_GROUP" -n "$SERVER_NAME" \
		-l "$LOCATION" --version "$PG_VERSION" --tier Burstable --sku-name Standard_B1ms \
		--storage-size 32 --admin-user "$ADMIN_USER" --admin-password "$ADMIN_PASSWORD" \
		--public-access "$CLIENT_IP" --yes -o none
fi

log "allow-listing extensions and preload libraries"
az postgres flexible-server parameter set -g "$RESOURCE_GROUP" -s "$SERVER_NAME" \
	--name azure.extensions --value "PG_STAT_STATEMENTS,HYPOPG,PG_HINT_PLAN" -o none
az postgres flexible-server parameter set -g "$RESOURCE_GROUP" -s "$SERVER_NAME" \
	--name shared_preload_libraries --value "pg_stat_statements,pg_hint_plan" -o none
log "restarting to load preload libraries"
az postgres flexible-server restart -g "$RESOURCE_GROUP" -n "$SERVER_NAME" -o none

log "running the documented setup SQL as $ADMIN_USER (CHECK-AZ-02)"
# ON_ERROR_STOP=0: a refused statement is a checklist finding, not a crash.
PGPASSWORD="$ADMIN_PASSWORD" AGENT_PASSWORD="$AGENT_PASSWORD" docker run --rm -i \
	-e PGPASSWORD -e AGENT_PASSWORD postgres:17-alpine sh -c \
	"psql -v ON_ERROR_STOP=0 -v agent_password=\"\$AGENT_PASSWORD\" \
	'host=$HOST user=$ADMIN_USER dbname=postgres sslmode=require'" <<'SQL'
SELECT format('CREATE USER sage_agent WITH PASSWORD %L', :'agent_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sage_agent') \gexec
SELECT format('ALTER USER sage_agent WITH PASSWORD %L', :'agent_password') \gexec
GRANT pg_monitor TO sage_agent;
GRANT pg_read_all_stats TO sage_agent;
GRANT CREATE ON SCHEMA public TO sage_agent;
GRANT pg_signal_backend TO sage_agent;
GRANT sage_agent TO sageadmin;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS hypopg;
CREATE EXTENSION IF NOT EXISTS pg_hint_plan;
CREATE SCHEMA IF NOT EXISTS sage AUTHORIZATION sage_agent;
SQL

log "granting the signed-in identity Contributor on the server (ARM parameter writes)"
SERVER_ID="$(az postgres flexible-server show -g "$RESOURCE_GROUP" -n "$SERVER_NAME" \
	--query id -o tsv)"
ASSIGNEE="$(az ad signed-in-user show --query id -o tsv 2>/dev/null || true)"
if [[ -n "$ASSIGNEE" ]]; then
	az role assignment create --assignee-object-id "$ASSIGNEE" \
		--assignee-principal-type User --role Contributor --scope "$SERVER_ID" -o none \
		2>/dev/null || log "role assignment skipped (already present or inherited)"
fi

log "done. Target recorded in $ENV_FILE"
log "next: scripts/azure/live-checklist.sh"
