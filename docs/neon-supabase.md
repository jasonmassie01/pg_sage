# Neon and Supabase

Use a persistent PostgreSQL connection for the sidecar. Neon direct endpoints and Supabase direct
endpoints have been tested against disposable hosted databases. Supabase's shared **session**
pooler on port 5432 is the IPv4 alternative when direct IPv6 connectivity is unavailable.

Neon pooled endpoints and Supabase transaction poolers are useful for application traffic, but
transaction pooling does not preserve the session state required by every pg_sage feature.
Startup locks, HypoPG, session settings and listeners need a direct or compatible session connection.
The startup guard also rejects Supabase's dedicated transaction pooler on port 6543.
Neon protocol prepared statements are supported by its pooler; the restriction is broader session
state, not all prepared statements. See [Neon pooling][neon-pool] and [Supabase connections][supa-connect].

## Connect

Put credentials in environment variables or your secret manager. For example, if your secret
manager already supplies NEON_DATABASE_URL:

```powershell
$env:SAGE_DATABASE_URL = $env:NEON_DATABASE_URL
```

Use SUPABASE_DATABASE_URL in the same way for Supabase. Include the provider's TLS options in
the URL. Supabase pooler usernames include the project reference; use the connection details from
the provider dashboard instead of changing only the host in a direct URL.

Alternatively configure SAGE_PG_HOST, SAGE_PG_PORT, SAGE_PG_USER, SAGE_PG_PASSWORD,
SAGE_PG_DATABASE and SAGE_PG_SSLMODE. SAGE_PG_MAX_CONNS bounds the sidecar pool. Start with a small
pool on Free plans and leave capacity for your application and provider services.

For a fleet, register each database endpoint independently and preserve its provider, database name,
trust level and connection settings. Provider detection recognizes the hosted endpoint domains.
Keep observation mode until findings and permissions have been reviewed.

## Capabilities and configuration

Both providers offer pg_stat_statements, HypoPG and pgvector. Extension installation, version,
namespace and privileges are discovered on the actual database. Supabase commonly installs
extensions in the extensions schema. A supported extension is not necessarily already installed.
Neon also lists pg_hint_plan; Supabase's current hosted extension list does not. Unavailable hints
must leave ordinary explain, index advice and vector workflows usable. [Neon extensions][neon-ext],
[Supabase extensions][supa-ext]

Neon and Supabase have no ordinary PostgreSQL superuser access. Portable maintenance actions on
owned objects retain pg_sage's approvals, trust ramps, replica checks, maintenance windows and
resource admission. User-context GUCs can use database/session/role scope. Recommendations use
the collected pg_settings.context before generating ALTER DATABASE SQL. Unknown or instance scopes
remain advisory; pg_sage does not send ALTER SYSTEM to these services. [Neon compatibility][neon-compat],
[Supabase configuration][supa-config]

Executable database setting recommendations require an explicit inverse for the same parameter.
An existing inverse SET value is preserved. Missing or invalid rollback information leaves the
change advisory until the prior database override is captured; RESET does not necessarily restore
the previous value.

Supabase documents preinstalled auto_explain and selected privileged role settings. Use its
supported log/configuration controls; do not assume host log files are accessible. Hosted metrics
can be supplied through the optional SAGE_SUPABASE_OBSERVABILITY_TOKEN integration when configured.
This is a management/observability credential, separate from the PostgreSQL password. Missing or
stale CPU/IO evidence must never be treated as zero utilization. [Supabase metrics][supa-metrics]

The Supabase integration polls management logs and parses auto_explain plans into the query store
and RCA input. It derives CPU utilization from core idle metrics and exposes memory evidence.
Data/WAL device utilization remains unknown, so actions requiring complete host-load admission
stay blocked. Malformed log records are reported without stopping valid records; transient storage
errors remain retryable. Neon Free did not expose the paid log/metrics export integration.

## Provisioning (removed)

pg_sage no longer creates, monitors or deletes Neon or Supabase projects and branches: that
provisioner was removed in G0. If an earlier version created resources for you, follow the
decommission runbook in `sidecar/internal/decommission/README.md`, which lists every resource
it may have created, with a delete template per provider. Governed short-lived branches return
as sandbox substrates in a later release.

## Verified limitations and remaining integrations

The Free Supabase organization used for verification reported no branch entitlement and no managed
backup retention. Free project lifecycle and
SQL schema isolation remain available within the account quota. Sending plan=free cannot enforce
billing: Supabase ignores that deprecated field and uses the organization plan. [Project API][supa-project],
[backup plans][supa-backup]

Neon Free rejected a custom suspend timeout.

The tested Neon role could install pg_hint_plan but could not load it or enable effective hints.
Capability reporting distinguishes installation from a loaded, enabled module. Supabase did not
offer pg_hint_plan in its extension catalog. These results apply to the tested projects and roles.

Backup checks describe completed backups or the configured restoration window and explicitly do
not claim a restore drill. Free Supabase needs logical exports for backup assurance.

## Backup and rehearsal evidence

Application-schema logical export and restore were verified live for both providers using
PostgreSQL 17 pg_dump and pg_restore. Fresh disposable local databases reproduced all 100 rows,
ordered content hashes including vectors, HNSW and other indexes, constraints, partition children
and identity sequence state. Negative foreign-key and check-constraint writes failed correctly.
This test excludes provider-owned schemas, roles and ACLs; it does not claim a complete platform
restore, automated restore drill or managed PITR execution.

The existing MCP snapshot factory remains unconfigured for all native providers;
its clone-unavailable path yields a recommendation rather than a successful rehearsal. These are
implementation gaps, not provider impossibilities. A configured external DLE provider remains a
separate option. Do not claim full migration rehearsal or automatic backup restoration from a
successful connection or create/status/delete test.

[neon-pool]: https://neon.com/docs/connect/connection-pooling
[supa-connect]: https://supabase.com/docs/guides/database/connecting-to-postgres
[neon-ext]: https://neon.com/docs/extensions/pg-extensions
[supa-ext]: https://supabase.com/docs/guides/database/extensions
[neon-compat]: https://neon.com/docs/reference/compatibility
[supa-config]: https://supabase.com/docs/guides/database/custom-postgres-config
[supa-metrics]: https://supabase.com/docs/guides/observability/metrics
[supa-project]: https://supabase.com/docs/reference/api/v1-create-a-project
[supa-backup]: https://supabase.com/docs/guides/platform/backups
