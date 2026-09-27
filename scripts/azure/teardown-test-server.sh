#!/usr/bin/env bash
# Delete the pg_sage Azure test resource group (server, firewall rules,
# role assignments) and the local env file. Irreversible: the server and
# its data are gone.
set -euo pipefail

ENV_FILE="${SAGE_AZURE_ENV_FILE:-$HOME/.pg_sage/azure-test.env}"
[[ -f "$ENV_FILE" ]] || { echo "no $ENV_FILE; nothing recorded to delete" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"

echo "This deletes resource group '$SAGE_AZURE_RESOURCE_GROUP' and everything in it." >&2
if [[ "${1:-}" != "--yes" ]]; then
	read -r -p "Type the resource group name to confirm: " answer
	[[ "$answer" == "$SAGE_AZURE_RESOURCE_GROUP" ]] || { echo "aborted" >&2; exit 1; }
fi
az group delete -n "$SAGE_AZURE_RESOURCE_GROUP" --yes --no-wait
rm -f "$ENV_FILE"
echo "deletion started; 'az group show -n $SAGE_AZURE_RESOURCE_GROUP' fails once it is gone" >&2
