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
trust level and connection settings. Provider detection recognizes the hosted endpoint domains and,
behind a custom domain, a `neon.*` setting (Neon) or the `supabase_admin` role (Supabase). To pin
the provider anyway, set `databases[].provider: neon` (or `supabase`); the override applies while
detection cannot tell and never replaces what it detects (see docs/managed-clouds.md).
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
the collected pg_settings.context before generating ALTER DATABASE SQL (superuser-context
settings only when the role holds SET on them). The executor captures the prior database setting
and rolls back to it, so no caller-supplied rollback is needed. Unknown or instance scopes
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
and RCA input. The log query names the monitored database, so a busy neighbouring database cannot
fill the 1000-row cap (a database name containing a backslash or a control character is read
unfiltered and filtered client-side). It derives CPU utilization from core idle metrics. Host memory
(total and available) feeds the advisor and tuning memory gates, such as the `work_mem` cap of 5% of
RAM and the memory-pressure guard, while it is less than 2 minutes old; older memory counts as
unknown. Supabase CPU and memory appear in `GET /api/v1/cloud-telemetry` with `provider:
"supabase"`, or as unavailable with a reason when no fresh metrics exist. In fleet mode each
Supabase database has its own collector and its own status entry. Data/WAL device utilization
remains unknown, so actions requiring complete host-load admission stay blocked. A log record with
a malformed timestamp or process ID is skipped and counted, with one warning per poll, while the
valid records of the batch are delivered and the cursor advances; transient storage errors remain
retryable. Neon Free did not expose the paid log/metrics export integration.

## Provisioning (removed)

pg_sage no longer creates, monitors or deletes Neon or Supabase projects and branches: that
provisioner was removed in G0. If an earlier version created resources for you, follow the
decommission runbook in `sidecar/internal/decommission/README.md`, which lists every resource
it may have created, with a delete template per provider. Governed short-lived branches return
as sandbox substrates in a later release.

## Provider advisors

Supabase performance advisors are read hourly with `SAGE_SUPABASE_OBSERVABILITY_TOKEN` for the
project the endpoint names (the token needs advisor read access). The index lints map to
pg_sage's findings: `unindexed_foreign_keys` (matched by constraint to `missing_fk_index`),
`unused_index` and `duplicate_index`; other lints (RLS, primary keys, bloat) are not index
advice and are not read. Each lint gets pg_sage's verdict (`agree`, `provider_only` with whether
pg_sage would act, `conflict`, `unknown`), shown in `GET /api/v1/provider-advice` and on the
finding's case. A 401 is recorded as the source's error and leaves earlier advice unchanged.
Neon has no advisor API, so pg_sage's own index findings stand alone there. See
[Provider advice](managed-clouds.md#provider-advice).

## Data API exposure

A table `anon` can read is reachable from the internet only when the project's Data API
(PostgREST) serves its schema, and SQL can't see which schemas it serves. With
`SAGE_SUPABASE_OBSERVABILITY_TOKEN` set, pg_sage reads it from the Management API
(`GET /v1/projects/{ref}/postgrest`, its `db_schema` list) for the project the endpoint names:
one read an hour, read only. The response also carries the project's JWT secret; pg_sage
decodes only `db_schema` and never keeps, logs or shows anything else.

AP-03 and AP-04 findings then carry `detail.data_api_exposed` for their schema:

| Value | Meaning | Severity |
| --- | --- | --- |
| `true` | the Data API serves the schema | unchanged (AP-03 stays critical) |
| `false` | the Data API does not serve the schema | critical drops to warning |
| `unknown` | not read: no token, a custom domain that names no project, a 401 or 429, or a response without `db_schema` | unchanged, never inferred safe |

- A 401 (or 403) and a 429 are each logged once until a read succeeds. Any failure waits an
  hour before the next read.
- A change of the served schemas re-runs the exposure detectors at the next exposure sweep,
  even when the catalog did not change.
- Projects created from 30 May 2026 no longer expose new tables in `public` to the Data API
  automatically. Where that shows as missing grants to `anon` and `authenticated`, AP-03
  already sees it. pg_sage reads no separate "expose new tables" setting: no such field was
  verified against a real project.
- Off Supabase the field is absent.
- The share of AP-03 findings with a known state is
  `count(*) FILTER (WHERE detail->>'data_api_exposed' IN ('true', 'false'))` over the open
  `agent_posture:AP-03` rows of `sage.findings`.

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

## Branch rehearsal on Neon

`clone.provider: neon_branch` rehearses migrations and index changes on a copy-on-write Neon
branch of production (lab-05b). Each rehearsal creates branch `sage-<purpose>-<8 hex>` from
`clone.neon.parent_branch`, or from the branch of the monitored endpoint (found by its host; a
`-pooler` host counts as its endpoint), with one read-write endpoint that autoscales from 0.25
to `clone.neon.endpoint_max_cu` compute units. A branch is typically ready in under a minute.
The clone connects as the monitored database's role with `sslmode=require`; its password is
never logged.

```yaml
clone:
  provider: neon_branch
  neon:
    project_id: shiny-wind-028834   # the monitored database's project
    # parent_branch: br-aged-salad-637688   # default: the monitored endpoint's branch
    # api_key: set SAGE_CLONE_NEON_API_KEY or SAGE_CLONE_NEON_API_KEY_FILE instead
    endpoint_max_cu: 1
    ttl_minutes: 60
```

**API key.** The key creates and deletes branches, so treat it as a write credential for the
project. Use a project-scoped key (an organization key limited to the project) where your plan
offers one, never a personal key with access to other projects. Without a key the provider is
unavailable: rehearsals are recommendation-only (`clone_unavailable`) and startup logs a warning.
A 401 or 403 from Neon names `clone.neon.api_key`. The API refuses `clone.provider: neon_branch`;
set it in the config file.

**Cleanup.** pg_sage asks Neon for an `expires_at` of now plus `clone.neon.ttl_minutes` on every
branch, as the Neon branch API documents; it has not been verified against a live project in
this build (OWNER ACTION). The receipt and its reaper are the backstop either way: a branch
whose create timed out is left `create_uncertain`, found by its name and deleted by the next
reaper pass, and a failed create deletes its branch before it returns. A create reply lost in
transport or a 5xx (which a gateway can send after Neon made the branch) is followed by a lookup
by name, and a branch found is deleted. A rehearsal ends within `clone.neon.ttl_minutes` when
that is shorter than the 1-hour rehearsal bound, so it never outlives its branch. pg_sage deletes only
branches named `sage-*` that are neither the default nor protected. Neon throttles branch
creation: a 429 or 5xx backs creates off (one minute doubling to an hour), and
`clone.max_creates_per_hour` caps creates per deployment. Each branch is billed for its compute
and written data while it lives.

**What it cannot do.** A branch starts with empty statistics, and Neon resets
`pg_stat_statements` when a compute suspends, so a rehearsal compares before and after on the
same branch only, never with production's counters. The rehearsal still checks that the clone
is not the production server before the first statement.

**Supabase gets no clone provider**: new Supabase branches start without data, so a rehearsal
on one would show neither realistic plans nor timings.

## Backup and rehearsal evidence

Application-schema logical export and restore were verified live for both providers using
PostgreSQL 17 pg_dump and pg_restore. Fresh disposable local databases reproduced all 100 rows,
ordered content hashes including vectors, HNSW and other indexes, constraints, partition children
and identity sequence state. Negative foreign-key and check-constraint writes failed correctly.
This test excludes provider-owned schemas, roles and ACLs; it does not claim a complete platform
restore, automated restore drill or managed PITR execution.

Without `clone.provider: neon_branch` (above), the clone-unavailable path yields a
recommendation rather than a successful rehearsal. These are
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
