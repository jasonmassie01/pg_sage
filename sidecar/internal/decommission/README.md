# Decommissioning AgentDB provisioning

pg_sage used to create databases for AI agents ("AgentDB"): local schemas and
databases, AWS RDS and GCP Cloud SQL instances, Neon and Supabase projects and
branches, and Databricks Lakebase branches. That provisioner was removed in G0
(AGENTDB-SPEC §12, decision D-1). pg_sage no longer creates, monitors, extends
or destroys those databases, and it holds no authority to delete them.

**pg_sage must never forget a billed resource it created.** This package is
what remains: a read-only inventory of everything the removed provisioner may
have created, a delete template per item, and your acknowledgement. You delete
resources yourself, with your provider's tools, after review.

## 1. Drain on the previous version

Before you upgrade, let in-flight creates and destroys settle on the last
version that had the provisioner. Rows stuck in `provisioning`,
`create_uncertain` or `destroying` are still inventoried after the upgrade, but
their provider state is then yours to check.

## 2. Review the inventory

On every start, if `sage.agent_db_deployments` exists on the control database
(the meta database, else the fleet's primary), pg_sage logs one line per item
under the `decommission` component and warns while any item is unacknowledged.
The same inventory is served as JSON to admins:

```
GET /api/v1/agentdb/decommission
```

Items are selected by **evidence of a live provider call**, not by status: a
row in `failed`, `destroyed` or `status_unknown` is listed when it ever had
`live_mode`, a `create_operation_id` or `provider_resource_id`, a live receipt,
a consumed live authorization, a live provider attempt or a live creation
receipt. Kinds:

| Kind | What it is |
|---|---|
| `provider_resource` | The cloud instance, project or branch. When no id was recorded (an uncertain create) the template uses the deterministic name the provisioner sent |
| `rds_final_snapshot` | Final snapshots a destroy of a kept (not disposable) RDS instance took, named `<instance>-final-<UTC time>` |
| `local_schema`, `local_database` | Schemas and databases pg_sage created on the control database's server; `present` says whether they still exist |
| `agent_sage_schema` | The `sage` schema pg_sage's own bootstrap left inside an agent database that fleet sync attached |

Each item lists provider, resource id, deterministic name, region, account or
project (`unknown` where never recorded), created_at, the evidence, and a
`delete_template`. Templates quote every value; still read each one before you
run it. Cloud SQL lifts deletion protection first; RDS makes you choose a final
snapshot (or `--skip-final-snapshot`); Neon and Supabase have separate project
and branch commands.

Fleet-synced agent databases are no longer monitored. To keep monitoring one,
register it as an ordinary fleet database (`databases:` or the Fleet page).

## 3. Remove credentials

The inventory's `credentials` list names every environment variable the
provisioner read (provider API keys and tokens, the Supabase database master
password, `PG_SAGE_AGENTDB_*` DSNs, the enable flags) and the IAM grants from
the old runbooks. Values are never read into the inventory. pg_sage warns at
startup while any of these variables still holds a value. Unset them, then
revoke or rotate the credentials at the provider.

## 4. Acknowledge

When every item is deleted or deliberately kept, acknowledge the items you
exported and handled, either through the API:

```
POST /api/v1/agentdb/decommission/ack
{"acknowledged_resources": ["provider_resource:<deployment id>", ...], "exported": true}
```

or in the config file:

```yaml
agentdb_decommission:
  acknowledged_resources: [provider_resource:<deployment id>]
  exported: true
```

Acknowledgements are recorded once per item in `sage.agentdb_decommission`
with the actor (your login, or `config:<path>`). An id that is not in the
current inventory is refused (API) or ignored with a warning (config).

## 5. Configuration

An `agentdb:` section in the config file is ignored with one warning. If it
sets `live_provisioning_enabled: true`, pg_sage refuses to start: remove the
section once you have followed the steps above. Persisted `agentdb.*` settings
in `sage.config` are deleted on upgrade, each with a `sage.config_audit` row
that keeps the old value.

## 6. The legacy tables

The 27 legacy tables (`LegacyTables`) stay in G0 because the inventory reads
them. G1 drops them by that explicit list once the acknowledgement exists or
the operational tables are empty (§12 step 5). The historical design notes,
runbooks and reports live under `reviews/archive/agentdb-2026-05/`.
