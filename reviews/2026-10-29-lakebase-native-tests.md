# Native Lakebase PG18 verification — 2026-10-29

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

> Historical acceptance/audit snapshot. For current dev.5 behavior and validation, see the [current acceptance matrix](2026-10-30-v2.5-dev5-acceptance.md) and [per-source native SRE matrix](2026-10-30-native-sre-source-matrix.md). Original results and failures below are retained.

Target: workspace profile `<test-workspace-profile>`, project `<test-project>`, branch `<native-test-branch>`, direct
endpoint `primary`, database `pg_sage_test`, selected native role
`<operator-email>`. This branch has a 48-hour TTL. All fixture DDL is
restricted to the explicitly isolated branch/database. Connections use verified
TLS and the actual production Databricks SDK OAuth pool hook.

This report supplements the [full local suite](2026-10-29-full-test-suite.md) and
the [platform compatibility assessment](2026-10-29-lakebase-compatibility.md).
It does not relabel local PostgreSQL coverage as Lakebase coverage.

## Results

The first complete native test run passed in 92.279 seconds on PostgreSQL
`18.6` (`server_version_num=180006`). The native role is not a true superuser.
Log: `.local-test-results/lakebase-native.log` (ignored local evidence).

The complete three-suite native run with race detection then passed in
201.983s: 30 pass events including subtests, zero skips/failures. Evidence is
`.local-test-results/lakebase-native-final.log`. This includes the improved
production database-setting inverse/readback path. The final connected-database
target restriction has additional meaningful local race coverage; it was added
after that native binary compiled. The final current-source full executor race run passed in 164.768s with
1,244 pass events and zero skips/failures
(`.local-test-results/executor-v25-final-guard.json`). A final native targeted
race run on that connected-database guard passed in 47.224s, including the exact
prior/no-prior inverse and unsupported-operation refusals
(`.local-test-results/lakebase-final-guard-live.log`). The monolithic rerun is
tracked separately.

| Area | Live result | Scope or limitation |
| --- | --- | --- |
| TLS, identity, direct endpoint | Pass | `verify-full`; actual selected native role |
| Schema/bootstrap/migrations | Pass | 128 durable `sage` tables; repeated bootstrap and fresh physical pool; existing config marker retained |
| Collector | Pass for asserted categories | Fresh persisted snapshots and explicit queries/tables/indexes/sequences assertions passed. The original log's final replication read was canceled at shutdown; it does not establish an empty unavailable map or every optional sensor |
| Analyzer | Pass | Completed production cycle; 16 fresh slow-query findings; rows survive pool restart |
| Owned maintenance SQL | Pass | ANALYZE, VACUUM ANALYZE, concurrent index CREATE/REINDEX/DROP, column statistics target, extended statistics, table reloptions and restoration |
| Database settings | Pass | `work_mem` at database scope, observed on a fresh session and exact original setting restored |
| pg_stat_statements | Pass | Installed 1.12; real per-database statistics read and collector use |
| pgcrypto | Pass | Installed 1.4; digest executed |
| pg_hint_plan | Unsupported in the tested configuration | Installed 1.8.0 but hook inactive; `LOAD` returns 0A000 and role lacks SET privilege for session_preload_libraries; product correctly reports installed_not_loaded |
| pgvector | Production vector lab pass | Installed 0.8.6; actual HNSW, exact baseline, three queries, recall 1.0, no automatic application |
| pgstattuple | Pass | Installed 1.5; owned table inspected |
| HypoPG | Unavailable | Absent from `pg_available_extensions`; hypothetical-index benefit verification is unsupported |
| pgAudit | Installation refused | Advertised in catalog but `CREATE EXTENSION pgaudit` returns SQLSTATE `42501`; audit log pipeline is unverified |
| Production operator/policy path | Pass | Real durable standing policy, decision ledger, action log, measured hygiene verification, index/statistics rollback; exact database setting prior/derived inverse and fresh-session forward+rollback readback |
| Combined native Lakebase + Unity Gateway advisors | Pass | All five full advisor pipelines, race detection, zero skips/failures, 47.404s; separate ephemeral native fixture DB created and cleanup verified |
| Real credential expiry | Retry pass, 3634.55s | Held native session healthy through 30s past actual 18:48:27Z expiry; production pool reset obtained a different backend with a second credential and advanced expiry. First run's pre-expiry network timeout retained below |
| Idle endpoint reconnect and durable readback | Pass, 8.077s with race detection | API ACTIVE→IDLE→ACTIVE; ordinary production OAuth connections woke the endpoint. Existing marker, 77 snapshots and 63 findings read in a verified-TLS read-only session; no endpoint updates or SQL mutation |

Initial unsuccessful native runs were fixture corrections: config keys use scoped
partial uniqueness, `pg_` schema names are reserved, PG18 represents the default
column statistics target as NULL, and orderly collector/analyzer cancellation
can emit cancellation error logs. The corrected suite requires a newly completed
analyzer cycle and fresh durable rows; shutdown cancellation is not counted as a
platform error. No product compatibility failure was concealed by a broad skip.

## Production support scope

Lakebase is being enabled explicitly for these contracts only:
`analyze_table`, `vacuum_table`, `create_index_concurrently`,
`revert_created_index`, `reindex_concurrently`, `create_statistics`,
`revert_created_statistics`, `alter_database_guc`.

The production operator run passed in 62.867s before the database-setting capture
improvement; the actual improved native database-setting tests then passed in
46.389s. Both no-override and existing 12MB cases supply a deliberately wrong
999MB caller rollback, assert the production-derived RESET/SET inverse instead,
assert fresh physical-session readback in after_state, and restore the exact
original catalog entry. Local race regressions additionally prove role-specific
overrides never become database-wide priors, masked settings are refused before
SQL, and a request targeting another database is refused without mutation.

The general portable-provider list stays unchanged. The SQL validator still
rejects VACUUM FULL and database/schema/system-wide REINDEX. Lakebase database
settings additionally require live `pg_settings.context='user'`, target the
connected database, and have no higher-priority role/client/session override;
unknown or unreadable settings fail closed. ALTER SYSTEM remains provider-refused.
User-index DROP, index replacement, role governance, backend signals, hint
application, retention and arbitrary table DDL are not enabled by this work.

Index/statistics execution and catalog validity establish the SQL path. They do
not establish a performance gain. Without measured before/after workload
evidence, no improved outcome may be invented or credited. HypoPG is absent.

## Legacy fixture audit

There are 93 package `TestMain` wrappers using `internal/testdb.Run`.
That harness creates package databases with `CREATE DATABASE ... TEMPLATE
template0`, installs extensions, runs raw `pgx` connections, then uses
`DROP DATABASE ... WITH (FORCE)`. The raw connections do not use the production
OAuth refresh hook. Passing a passwordless Lakebase DSN to those unchanged
fixtures does not test Lakebase compatibility.

| Legacy fixture requirement | Concrete examples | Disposition |
| --- | --- | --- |
| True-superuser system catalog writes | `internal/executor/manual_safety_test.go` updates `pg_index.indisvalid` | Cannot run unchanged on managed Lakebase; product-owned concurrent-index validity is tested through real SQL instead |
| Cluster-wide statistics resets | collector/analyzer/optimizer fixtures invoke `pg_stat_statements_reset`, `pg_stat_reset` | Would affect inherited databases on the branch; excluded from this native target, not relabeled as passing |
| HypoPG session-local hypothetical indexes | `internal/optimizer/hypopg_session_test.go` and related admission tests | Extension unavailable; optimizer what-if remains explicitly unsupported |
| Global settings, reloads, special postmaster configuration | config roundtrip, low pg_stat_statements capacity, pgaudit preload fixtures | Managed instance settings cannot be supplied; production refusal must pass |
| Server filesystem/log access and JSON log ingestion | `internal/pgaudit/TestRealPGAuditCorrelation`; logwatch/auto_explain fixtures | No direct server OS/log-file access; local dedicated-server coverage does not establish native Lakebase log ingestion. Beta `system.lakebase.plan_history` and `postgres_logs` are a supported provider surface, but pg_sage currently lacks their connector |
| Cluster roles, SCRAM passwords, arbitrary privilege fixtures | agent guard, broker, posture and vector permissions fixtures | Lakebase has limited role management; each native path needs separate evidence. Workspace OAuth cannot substitute for brokered native-SCRAM roles |
| Physical standbys, promotion, restartable servers or PgBouncer admin | HA/causal/pooler dedicated fixture tests | Topology absent; these remain unverified on the single native test endpoint |

The native suite therefore uses a dedicated opt-in package rather than a broad
"managed provider" skip. Unsupported probes produce explicit classifications;
unrun legacy paths remain unverified. All five previously skipped full advisor
workflows now pass through native Unity Gateway with race detection both against
synthetic fixtures on a separate local PG17 server (42.138s) and against a new
native Lakebase PG18 fixture database (47.404s). The ephemeral native fixture was
observed and cleanup verified against pre-run catalog inventory. Its legacy raw
pgx test harness used a freshly minted environment-only OAuth credential for the
bounded run; production OAuth refresh remains separately exercised by the native
pool tests. Synthetic rewrite scenario metrics are not a database performance
measurement. Evidence: `.local-test-results/advisor-lakebase-unity-gateway.json`
and `advisor-lakebase-unity-gateway-summary.json`. The Gemini-specific arm remains
unrun.

The combined native run selected exactly `TestBloatE2E_FullPipeline`,
`TestConnE2E_FullPipeline`, `TestFullPipeline_AllAdvisorsRun`,
`TestRewriteE2E_FullPipeline`, and `TestWALE2E_FullPipeline`. Each test requires
successful billed inference through the same `system.ai.gpt-6-sol` service;
the combined case additionally requires separate token growth for all four
advisors (WAL440, connection567, rewrite654, bloat660 tokens). The observed fixture
database was `pgsage_internal_advisor_17349_0ebb1c60`, with **zero leftovers**
according to the before/after database catalog inventory. Preflight review
confirmed no cluster statistics reset, role change or instance-setting mutation.
All extension/table DDL and synthetic DML stayed in that fresh database.

These tests collect fixture/catalog context and invoke advisor functions without
persisting findings or executing model recommendations. The fresh fixture has
no `sage.findings`, so the rewrite dedup query warns and degrades; this run does
not establish that dedup/persistence path. Native migrations, persistence and
reviewed-action tests above supply separate evidence. The raw fixture uses a
single fresh OAuth credential for this sub-minute run, rather than the production
hourly-refresh hook. The credential and full DSN were never saved.

## Sources and shipped behavior

Public documentation was checked against actual PG18 behavior:

- [Public PostgreSQL compatibility](https://docs.databricks.com/aws/en/oltp/projects/compatibility): PG16/17/18, managed restrictions, supported setting scope.
- [Public extensions](https://docs.databricks.com/aws/en/oltp/projects/extensions): cross-checked against actual installed versions and failed pgAudit installation.
- [Public authentication](https://docs.databricks.com/aws/en/oltp/projects/authentication) and [external connections](https://docs.databricks.com/aws/en/oltp/projects/external-apps-connect): credential lifetime and refresh on new physical connections.
- [Public scale-to-zero behavior](https://docs.databricks.com/aws/en/oltp/projects/scale-to-zero): automatic suspension after inactivity and resumption on a connection; sessions and in-memory state are not durable, while data and configuration persist.

The current public documentation and this deployed PG18.6 endpoint establish the observed PostgreSQL version support. Catalog advertisement did not
make pgAudit installable for this role; the live refusal takes precedence over
that apparent capability. Public listing of pg_hint_plan likewise does not establish that its hook can activate: the live refusal establishes the current limitation for the tested endpoint and role.

## Reproduction

Set the selected Databricks profile, passwordless verified-TLS DSN and exact
endpoint resource path in the environment; never persist a short-lived token.
The fixture guards require branch `<native-test-branch>` and database `pg_sage_test`.

```sh
PG_SAGE_LIVE_LAKEBASE_MUTATE=1 go test ./tests/lakebaselive -v -count=1 -timeout=8m
PG_SAGE_LIVE_LAKEBASE_SOAK=1 go test ./internal/databricksauth -run '^TestLakebaseLiveCredentialExpiry$' -v -count=1 -timeout=90m
PG_SAGE_LIVE_LAKEBASE=1 PG_SAGE_LIVE_LAKEBASE_READBACK=1 go test -race ./internal/databricksauth -run '^TestLakebaseLive(OAuthReconnect|PersistenceReadback)$' -v -count=1 -timeout=3m
```

The first real-expiry attempt failed after 746.56s, following eleven healthy
minute probes, with a network read timeout about 47 minutes before expiry. Its
cause remains unclassified; it is not evidence of an expiry-triggered failure.
That unsuccessful observation remains in
`.local-test-results/lakebase-expiry-soak.log`.

The independent retry passed in 3634.55s (package 3635.187s), with zero skips or
failures. Its first credential was minted at 17:48:27Z and actually expired at
18:48:27Z. The held native session answered every minute probe through 30s after
expiry. At/after 18:48:57Z, resetting the production pool produced a different
backend PID, a second credential mint and an advanced expiry. Evidence:
`.local-test-results/lakebase-expiry-soak-retry.log`.

The soak establishes constant-query held-session liveness and a newly
authenticated backend after a real credential expiration. It does not establish
held-session table-permission/UC-claim lifetime, compute suspend/resume, failover
or multi-day reliability. Native object reads/writes are established by the
separate fixture and App tests; `SELECT 1` alone makes no claim about table
authorization. This exact-expiry test uses the selected user identity, not the
deployed App service principal.

After the expiry test closed all pools, no SQL was issued while the native
endpoint's existing 300s inactivity timeout elapsed. Read-only endpoint metadata
showed `ACTIVE` at 18:53:10Z and `IDLE` at 18:54:09Z. The API's observed label is
`IDLE`; this report does not invent a `SUSPENDED` API label. Ordinary production
OAuth connections then passed `TestLakebaseLiveOAuthReconnect` in 5.06s and
`TestLakebaseLivePersistenceReadback` in 1.24s. The package passed with race
detection in 8.077s, two tests, zero skips/failures. Metadata showed `ACTIVE` again
at 18:54:46Z. The readback retained the existing `persistent` marker and read 77
snapshots and 63 findings, with verified TLS, expected user and a read-only
transaction. No endpoint-setting update, explicit restart or SQL write was used.
Evidence: `.local-test-results/lakebase-resume-readback.log` and
`lakebase-suspend-resume-observations.jsonl`.

This establishes reconnect and existing object readback after the observed
idle-state transition. It does not establish uninterrupted held sessions across
suspension, reset behavior of every statistics view, failover or multi-day
reliability. The new opt-in persistence test was added after the final local
monolithic suite's inventory; this separate successful live race run supplies
its coverage. Its exact isolated-endpoint skip is recorded in `.skip-allowlist`.
