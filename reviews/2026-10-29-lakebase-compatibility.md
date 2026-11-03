# Lakebase and Unity Gateway compatibility validation — 2026-10-29

Environment identifiers and private-source material are omitted from this public report. Recorded outcomes, versions and artifact hashes retain their original scope.

> Historical acceptance/audit snapshot. For current dev.4 behavior and validation, see the [current acceptance matrix](2026-10-30-v2.5-dev4-acceptance.md) and [per-source native SRE matrix](2026-10-30-native-sre-source-matrix.md). Original results and failures below are retained.

**Verdict:** the development integration passes live OAuth/TLS, native PG18 migrations and persistence, eight scoped action contracts, and GPT chat/JSON/tools. The [native test report](2026-10-29-lakebase-native-tests.md) and [deployed App acceptance](2026-10-29-databricks-app-acceptance.md) record the later isolated validation. The App runs on `<project>/<development-branch>` with its own service principal; deployed login, dashboard, Gateway and restart/persistence checks passed. The user-identity exact-expiry soak passed. The separate App-principal elapsed-time reconnect, uncached Gateway and database write/read/restore checks passed after 65 minutes on the same process; final redeployment, configuration and Chrome dashboard acceptance also passed. This is qualified compatibility with explicit managed-platform restrictions, not failover or full feature-parity certification.

**Dev.3 update, October 30:** [current build/acceptance and detailed feature matrix](2026-10-29-v2.5-dev3-acceptance.md) records the installed/deployed revision, scoped history connector, real native and App SRE investigations, signed runbook, Databricks-only OAuth controls and remaining functional gaps. Final App deployment is `01f1d472793f135aa1802c4e4ef7c979`, RUNNING/ACTIVE. The local composite has 16,271 passes/50 allowed skips/zero failures; native Lakebase has 46 passes/three delivery skips. All ten native telemetry projections and six App metadata streams queried successfully, but selected-resource delivery remains zero/unverified. Hints, HypoPG, historical SLO/RCA/SRE bridges and SRE maintenance breadth retain explicit limits. The inventory and dev.1/dev.2 evidence below remain historical to their stated runs.

## Scope and method

Workspace explicitly selected for this test: `<test-workspace-profile>` (`<deployment-url>`). Inventory: **8 projects, 12 branches, 21 databases**. Read-only connections were attempted to every inventoried database: **18 succeeded; 3 failed authentication**, all in the same service-principal-owned project. Successful connections cover 7 projects and 10 branches.

The initial inventory SQL probes used `BEGIN READ ONLY`, a 5-second statement timeout, a 1-second lock timeout, bounded catalog pages, and rollback. Existing schemas/data/settings/extensions were not modified. Normal database connections automatically resumed idle computes; no explicit unarchive/update/restart API was called during the survey. Later authorized mutation and App tests use fresh `<native-test-branch>` and `<development-resource>` branches and dedicated databases, as recorded in the linked reports. Authentication tokens stayed in process memory and subprocess environment, and are absent from the report and evidence files. No real database query text or row data was sent to an LLM; gateway tests used synthetic prompts and synthetic evidence only.

The psql survey used `sslmode=verify-full` with `sslrootcert=system`. The Go production-pool tests independently verified TLS and identity. A missing local `~/.postgresql/root.crt` initially prevented libpq verification; using the system trust store resolved it without weakening TLS verification.

## Live database matrix

Project, branch and database labels in this inventory are stable pseudonyms.

| Project / branch | Database | PostgreSQL | Connection/catalog probes | pg_stat_statements | sage schema |
|---|---|---|---|---|---|
| project-01 / branch-01 | `database-01` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-02 / branch-02 | `database-01` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-03 / branch-03 | `database-01` | 16.15 (f37ae9a) | PASS | Not installed | Absent |
| project-04 / branch-04 | `database-01` | 16.15 (f37ae9a) | PASS | Not installed | Absent |
| project-05 / branch-05 | `database-01` | 16.15 (f37ae9a) | PASS | 1.10 | Absent |
| project-05 / branch-05 | `database-02` | 16.15 (f37ae9a) | PASS | Not installed | Absent |
| project-05 / branch-06 | `database-01` | 16.15 (f37ae9a) | PASS | Not installed | Absent |
| project-05 / branch-06 | `database-03` | 16.15 (f37ae9a) | PASS | Not installed | Absent |
| project-05 / branch-06 | `database-04` | 16.15 (f37ae9a) | PASS | Not installed | Absent |
| project-06 / branch-07 | `database-01` | 18.6 (4e5fd08) | PASS | Not installed | Absent |
| project-01 / branch-01 | `database-05` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-01 / branch-01 | `database-06` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-01 / branch-01 | `database-07` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-02 / branch-02 | `database-08` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-02 / branch-08 | `database-01` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-02 / branch-08 | `database-08` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-07 / branch-09 | `database-01` | 17.11 (0bfbbeb) | PASS | Not installed | Absent |
| project-06 / branch-10 | `database-01` | 18.6 (4e5fd08) | PASS | Not installed | Absent |
| project-08 / branch-11 | `database-01` | 17 | AUTH FAILED | Unverified | Unverified |
| project-08 / branch-12 | `database-01` | 17 | AUTH FAILED | Unverified | Unverified |
| project-08 / branch-12 | `database-09` | 17 | AUTH FAILED | Unverified | Unverified |

The three authentication failures do not prove protocol incompatibility. `project-08` is owned by a service principal; the selected user identity could obtain a credential but could not authenticate as a Postgres role there. No roles or permissions were changed to bypass that boundary.

All 18 successful sessions reported `transaction_read_only=on`, `databricks_superuser` membership, `pg_monitor` membership, no real Postgres superuser, and database CREATE privilege. The nonexistent `sage` schemas mean this survey does **not** validate startup migrations or persistent state writes in these existing databases. An App service principal has separate identity and grants, so user-role success does not establish deployed-App permissions.

## Catalog and collector coverage

All 18 successful databases passed the actual collector projections for table statistics, index statistics, foreign keys, replication replicas/slots, PG16+ I/O statistics, partition inheritance, prepared transactions, index-definition watermark, and version-specific checkpoint counters. Field evaluation was forced, and only counts/payload lengths were retained for workload-related probes. `EXPLAIN (FORMAT JSON) SELECT 1`, `pg_is_in_recovery()`, `pg_current_wal_lsn()`, and `pg_database_size()` also succeeded. Zero-row replication and prepared-transaction results verify SQL compatibility and permissions, but do not reproduce actual incidents.

Additional bounded probes covered the full system-stat projection, sequence-page logic, top-five exact table/index sizes, and index definitions. On the existing PG16 database with `pg_stat_statements` installed, the collector's combined WAL/planning-time query-stat aggregation returned 25 evaluated rows. PostgreSQL 17/18 query-stat collection is **not** live verified against existing databases because the extension is not installed there.

`pg_stat_statements` is available on all tested versions, but was installed in only **1 of 18 accessible databases**: `wiki-search / wiki-search-branch / databricks_postgres`, version 1.10. Query text was visible for all sampled entries in that database, with no `<insufficient privilege>` rows. Missing extension errors elsewhere are prerequisites, not evidence of an unsupported extension. No extension was installed as part of the survey.

## Production authentication tests

`TestLakebaseLiveOAuthReconnect` passed against PG16.15 (`project-04`), PG17.11 (`project-01`), and PG18.6 (`project-06`). It used the actual `databricksauth.ConfigurePool` production hook with a passwordless DSN and the selected unified-auth profile. Each test opened a read-only TLS connection, checked identity, reset the pool, then established a new physical backend. Logs: `.local-test-results/native-lakebase-pg16.log`, `native-lakebase-pg17.log`, `native-lakebase-pg18.log`.

This initial test validates fresh connection credentials and reconnect behavior. Actual one-hour token expiry is separately established by the later native soak below; HA failover and a long running session across endpoint suspension remain fault-test coverage gaps.

### Credential expiry, held sessions, and App identity

Current [public authentication documentation](https://docs.databricks.com/aws/en/oltp/projects/authentication), fetched directly on October 29, explicitly states: "Token expiration is enforced only at login. Open connections remain active even after the token expires." OAuth database credentials expire after one hour. Separately, all authentication methods have a **24-hour idle timeout** and **3-day maximum connection life**; connections older than three days *may* be closed regardless of activity. These limits do not guarantee uninterrupted connectivity, and the documentation recommends connection retry handling. No concrete source was found for a general 10–15 minute direct-connection lifetime limit.

The held-session tests below cover the tested PostgreSQL grants; they do not establish every authorization mode.

The first real expiry-soak attempt failed after **746.56 seconds**, following eleven healthy minute probes, with a TCP network read timeout. Its credential was still approximately 47 minutes from expiry. This is a failed continuity observation before expiry, **not** evidence that OAuth expiry terminated the session and not a successful expiry test. Its cause remains unclassified. The original evidence remains in `.local-test-results/lakebase-expiry-soak.log`. The independent retry **passed in 3634.55s**, with zero skips/failures (`lakebase-expiry-soak-retry.log`). Its first credential was minted at `17:48:27Z` and expired at `18:48:27Z`; the held native session answered every minute probe through 30s beyond that actual expiry. At/after `18:48:57Z`, a production pool reset produced a different backend PID, a second minted credential and an advanced expiry. See [the native test report](2026-10-29-lakebase-native-tests.md) for the precise scope.

The soak keeps one physical session and probes it with `SELECT 1`, then resets the pool after real credential expiry, requires a different backend PID, and checks that a second credential was minted with a later expiry. These assertions distinguish **held direct-session liveness** from **fresh-session credential refresh**. A constant-expression probe does not verify table GRANT permissions or UC-only claims across expiry. The test runs through the production credential-cache/connection hook using the explicitly selected user identity, not the App service principal.

After that test closed its pools, the native endpoint's unchanged 300s inactivity timeout was allowed to elapse without SQL. Read-only API observations showed `ACTIVE` at 18:53:10Z, `IDLE` at 18:54:09Z, and `ACTIVE` at 18:54:46Z after ordinary production OAuth connections. The reconnect and read-only persistence tests both passed with race detection (8.077s package time, two tests, zero skips/failures); the existing marker, 77 snapshots and 63 findings were readable with verified TLS and the selected native identity. No endpoint update, explicit restart or SQL write was used. Evidence is `lakebase-resume-readback.log` and `lakebase-suspend-resume-observations.jsonl`. The API label observed was `IDLE`, not `SUSPENDED`. This establishes reconnect and object readback after the observed idle transition, not survival of held sessions across suspension or every statistics-reset behavior. The [public scale-to-zero documentation](https://docs.databricks.com/aws/en/oltp/projects/scale-to-zero) describes inactivity suspension, connection-triggered resume, durable data/configuration and loss of session/in-memory state. The new opt-in readback test was added after the final local monolithic inventory and is covered by this separate live race run.

The public [Databricks Apps tutorial](https://docs.databricks.com/aws/en/oltp/projects/tutorial-databricks-apps-autoscaling) explicitly distinguishes local user authentication from the deployed App service-principal context. The [external-connection guide](https://docs.databricks.com/aws/en/oltp/projects/external-apps-connect) describes 60-minute workspace and database credentials, with refresh when new physical connections are created. Therefore the deployed App needs its own same-process, hour-boundary validation: remain running for at least 65 minutes, then establish a fresh backend as its App service principal and verify database read/write persistence. Held sessions and health checks alone do not establish App credential rotation. The App test passed after 65 minutes with unchanged process/deployment, fresh physical backend, original session readback and bounded cache persistence followup, followed by uncached Gateway token-spend growth and a scoped configuration write/read/exact restore. The original cache assertion incorrectly exceeded its 60-minute TTL and is retained as a failed fixture; the restart-bound budget setting was also initially misclassified by a followup assertion. Corrected runtime proofs and their precise qualifications are in the App acceptance report. Without exposed App credential timestamps, this elapsed-time evidence cannot assert exact mint/expiry advancement.

## Feature limitations and prerequisites

| Capability | Observed / documented status | Consequence |
|---|---|---|
| HypoPG | Absent from `pg_available_extensions` on tested PG16, PG17, PG18 | Hypothetical-index verification is unavailable. Existing optimizer code marks recommendations unverified; preserve operator approval gates. |
| pg_stat_statements | Available 1.10 / 1.11 / 1.12 by major version; initially installed in only one surveyed database; 1.12 subsequently installed/tested in the isolated PG18 fixture | Explicit setup is needed per database. |
| pg_hint_plan | Installed 1.8.0 on isolated PG18, but hook inactive; LOAD and session preload refused | Hint application is unsupported in this configuration despite catalog availability. |
| pgstattuple / vector | Owned-table inspection and production HNSW vector lab passed on isolated PG18 | These synthetic tests do not establish every extension feature or broader workload performance. |
| pg_cron / pg_repack | Absent from live available-extension catalogs | Do not advertise their workflows as supported. |
| pgaudit | Advertised in live catalogs; installation on isolated PG18 refused with SQLSTATE 42501 | Audit-log integration remains unverified; catalog advertisement does not establish usable access. |
| Instance GUC changes | Public docs disallow instance-level configuration; observed `shared_buffers` / `max_connections` are postmaster context, `max_wal_size` / `autovacuum` sighup context | Generic `ALTER SYSTEM` or instance-level tuning must not run on Lakebase. |
| User-context settings | `work_mem`, `maintenance_work_mem`, `random_page_cost`, `effective_cache_size` show user context; production `work_mem` apply/exact rollback passed on isolated PG18 | Database-setting actions require the connected database, supported user context, ownership and unmasked readback. Other setting changes need their own evidence. |
| I/O and function timing | `track_io_timing=off`, `track_functions=none`, both superuser context | Missing timing must not be interpreted as evidence of zero work. |
| Host CPU / disk utilization and logs | No direct host operating-system or log-file access; Beta `system.lakebase` provides plans, structured Postgres logs and compute telemetry | Dev.3 adds an explicit scoped history connector and plan-cache/optimizer consumers. Selected-resource plan/log delivery remains unverified; historical telemetry cannot admit live host-dependent actions. See [Advanced Telemetry and SRE assessment](2026-10-29-lakebase-advanced-telemetry.md). |
| pg_stat_statements reset | Public docs: query stats lost on restart / suspension | Counter deltas, forecasting, and before/after comparisons must detect reset boundaries. |
| Cumulative statistics | Public docs: survive graceful stop/scale-to-zero; reset on failure/HA or >16MB | Do not confuse these with pg_stat_statements persistence. |
| Transaction pooler | Current public authentication docs state that built-in PgBouncer does not support OAuth; native password roles are required | Native OAuth configuration rejects endpoint metadata's pooled hosts. Direct endpoints are the tested native path. |

## Unity Gateway live validation

Unified API base URL: `https://<workspace>/ai-gateway/mlflow/v1`. The model is its fully qualified UC model-service name. Services were discovered with `databricks ai-gateway list-model-services --parent schemas/system.ai --view FULL --profile <test-workspace-profile>`; the OpenAI-style `/mlflow/v1/models` route returned HTTP400 as an unmanaged API path and is not the discovery interface.

| Model / operation | Result | Required configuration |
|---|---|---|
| `system.ai.claude-sonnet-4-5`, plain chat | HTTP200 | Supported |
| Same Claude, forced synthetic function tool | HTTP200 | Supported |
| Same Claude, `response_format: json_object` | HTTP400: unsupported response format for this model | Use `json_mode: false` and validated prompted JSON, or choose a JSON-mode-capable model |
| `system.ai.gpt-6-sol`, plain chat | HTTP200 | `max_completion_tokens` is supported |
| Same GPT, JSON object response format | HTTP200, valid JSON | Supported |
| Same GPT, tool call without reasoning override | HTTP400, requests `reasoning_effort: none` | Existing pg_sage automatic wire adaptation handles this |
| Same GPT, pg_sage native-auth live tests | Both tests PASS | Unified authentication and automatic tool-wire adaptation |

The actual Go tests `TestChatWithToolsLive_RealProvider` and `TestChatLive_RealProvider` both passed using `SAGE_LLM_AUTH=databricks`, no configured static API key, and `DATABRICKS_CONFIG_PROFILE=<test-workspace-profile>`. The tool test verified both the initial function call and the second turn consuming a synthetic tool result; the JSON test verified structured output. Total reported usage was 165 + 216 tokens for tool turns and 34 tokens for JSON chat. Command/output are recorded in `.local-test-results/native-unity-gateway.log`.

The five full advisor E2Es also passed with `-race`, native Gateway GPT, and no
Gemini key: bloat, connection, all advisors, rewrite, and WAL (42.138 seconds,
zero skips). Each requires real model token usage, and the all-advisors case
checks usage separately for each advisor. Evidence is in
`.local-test-results/advisor-unity-gateway-final.json`. These tests used a
disposable local PostgreSQL 17 database with synthetic fixtures; rewrite
eligibility used explicitly synthetic scenario metrics. They validate the
advisor-to-Gateway workflow, while native Lakebase database execution is covered
by the separate Lakebase live suites.

The same five tests subsequently passed **together on native Lakebase PG18 and
Unity Gateway GPT**, with `-race`, in **47.404 seconds**, zero skips/failures.
Each completed real billed inference through `system.ai.gpt-6-sol`; the combined
all-advisors test separately recorded usage for WAL, connection, rewrite and
bloat (440, 567, 654 and 660 tokens). A preflight audit found no cluster-wide
statistics reset, role change or instance-setting mutation in these selected
tests. `testdb.Run` created only the disposable
`pgsage_internal_advisor_17349_0ebb1c60` database on the authorized test branch;
the wrapper observed that database during the run and verified the final
prefixed-database inventory exactly matched the original inventory. Its fixture
table and four available extensions existed only in that disposable database.

This narrowly bounded fixture run supplied one freshly minted OAuth credential
in subprocess environment to the existing raw `testdb`/`pgx` connection path;
it does **not** exercise hourly refresh through the production connection hook.
The credential and full DSN were not saved. Synthetic rewrite scenario metrics
remain scenario inputs, not measured Lakebase performance. These tests collect
fixture/catalog context and invoke advisor functions; they do not execute model
recommendations or test persisted advisor findings. The rewrite dedup lookup
logged the expected missing `sage.findings` warning in the unmigrated fresh
fixture, so that persistence/dedup path is not established by this run. Separate
native suites cover migrations, persistence and reviewed actions. Evidence:
`.local-test-results/advisor-lakebase-unity-gateway.json` and
`advisor-lakebase-unity-gateway-summary.json`.

The dashboard model picker now uses `databricksauth.ListModelServices` through
the native SDK instead of the unsupported `/models` route. Both GET and request-
scoped POST discovery honor committed `llm.auth: databricks` without an API key.
Listings use the selected model's catalog/schema (default `system.ai`), follow
SDK pagination, filter to `mlflow/v1/chat/completions` support, and return the
actual fully qualified service IDs. Native preview endpoints must keep the
configured HTTPS origin; the SDK helper also validates the workspace origin.
Existing generic API-key discovery keeps its DNS-pinned endpoint checks.
`TestGatewayModelDiscoveryLive` passed with unified authentication and returned
**53 accessible chat model services**, including the selected GPT model. Synthetic
SDK/handler tests cover pagination, filtering, active auth snapshots, request
scope, and rejecting endpoint changes before discovery. This listing performs
no inference and has a ten-second request budget.

An external BYOK model-provider service uses a different native path (`/ai-gateway/openai/v1`) and requires `Databricks-Model-Provider-Service: catalog.schema.service`. That path/header combination was documented but not exercised. This distinction resolves older internal advice to use an OpenAI-native base URL; it does not replace the unified model-service route tested here.

## Documentation cross-check

Current public documentation was checked against the deployed system.

- Public [Postgres compatibility](https://docs.databricks.com/aws/en/oltp/projects/compatibility), updated October 27, documents PG16/17/18, statistics persistence differences, no real superuser/OS/log access, and parameter restrictions. Current public documentation and successful PG18 tests establish the observed version support, within the tested roles and scope.
- Public [Postgres extensions](https://docs.databricks.com/aws/en/oltp/projects/extensions) and [pg_stat_statements](https://docs.databricks.com/aws/en/oltp/projects/pg-stat-statements) support the extension/prerequisite and lifecycle findings.
- Public [Postgres roles](https://docs.databricks.com/aws/en/oltp/projects/postgres-roles) documents `databricks_superuser` inheriting `pg_monitor` and the identity-specific roles/grants model.
- Public [Query model services](https://docs.databricks.com/aws/en/ai-gateway/query-model-services) and [Query external model providers](https://docs.databricks.com/aws/en/ai-gateway/query-model-provider-services) document the separate unified and native API paths.
- Public [Connection pooling](https://docs.databricks.com/aws/en/oltp/projects/connection-pooling) documents transaction-mode limitations; use returned `status.hosts` endpoint names rather than reconstructing generic documentation examples.
- Public [Azure Lakebase Private Link](https://learn.microsoft.com/en-us/azure/databricks/oltp/projects/private-link) specifies `*.database.<region>.azuredatabricks.net`; AWS live API hosts use `*.database.<region>.cloud.databricks.com`. Azure hostname classification is source-backed but was not live tested here.

## Acceptance status after the initial survey

1. Completed: isolated PG18 branch/database, startup migrations, durable collector/analyzer state, `pg_stat_statements`, and production operator/rollback testing. Full destructive self-managed fixtures stay on disposable local servers.
2. Completed: Databricks App deployment, App-principal Lakebase/Gateway permissions, health, real browser login/dashboard, and persistence across pg_sage restart and full Apps stop/start. Final rebuild acceptance is tracked separately from the first deployment.
3. Real user-identity credential expiry passed in 3634.55s, including held-session liveness through expiry+30s and a second credential/new backend after expiry. Preserve the original pre-expiry network failure. The separate App service-principal elapsed-time reconnect, uncached Gateway and database write/read/exact restore checks also passed after 65 minutes on the same process; their evidence does not expose exact token mint/expiry.
4. Unverified: explicit reset/failover faults, held-session survival across suspension and UC-only expired-claim authorization. HypoPG, broker authentication on an OAuth-only project, direct host/log-file access and instance-setting workflows have the restrictions documented above. Lakebase's available system-table telemetry is not connected to pg_sage, and a native SRE investigation remains unverified; see the [telemetry assessment](2026-10-29-lakebase-advanced-telemetry.md) and [SRE audit](2026-10-29-lakebase-sre-coverage.md). Observed idle-transition reconnect and durable readback passed as described above. External provider and topology suites are separate coverage; skips are not passes.

Local evidence under `.local-test-results/`: `lakebase-preflight.json`, `lakebase-collector-extra.json`, `unity-gateway-claude.json`, `unity-gateway-gpt.json`, and native Go test logs. Evidence contains metadata/counts and synthetic model responses, without credentials or application row data.

## Independent implementation review

The native pool hook checks endpoint metadata against the configured host,
refuses plaintext fallback connections, composes an existing `BeforeConnect`
hook, and refreshes an in-memory credential within two minutes of expiry. The
SDK's workspace origin also bounds model discovery and LLM credentials. The
live reconnect and chat/JSON/tool tests above exercise these production paths;
the unit refresh test simulates expiry and concurrent connection growth.

The initial native LLM signer validates the exact HTTPS workspace origin.
Final review additionally identified that Go's default redirect behavior does
not enforce that boundary. Native chat, retries, and tool calls now use a
request-scoped client that rejects redirects; generic API-key redirect behavior
is preserved. Focused race tests cover both request paths and the existing
API-key behavior. A fresh full `go test -race ./internal/llm -count=1` passed
after this change (22.604 seconds). Both native GPT live tests were rerun and
passed through the updated transport, including the two-turn tool flow and
JSON output (6.705 seconds); the synthetic token counts remained 165 + 216 and
34. Evidence: `.local-test-results/native-unity-gateway-redirect-fix.log`.

Source review confirms that standalone monitoring, metadata connections,
YAML fleet startup/reload, and stored fleet attachment apply the native pool
hook. The live tests validate that hook, not full migrations or every fleet
lifecycle against Lakebase. Dashboard database connection-test/preview helpers
now use the same native mapping, including replacing bootstrap passwords.
The logical endpoint hostname is retained for endpoint authorization and TLS
SNI; the preview's lookup is pinned to its server-validated IP, preserving its
private-address policy and preventing a second DNS resolution. The actual
preview and saved-connection paths passed on `search / pg-sage-test / pg_sage_test`
with a passwordless, verify-full DSN. Configured-replica administrative
connections now apply the same native mapping. A connection/bootstrap-only live
test verifies TLS against the PG18 direct endpoint; it does not exercise standby
session termination or role revocation. Agent broker logins retain distinct
per-role SCRAM credentials, which cannot be replaced with workspace OAuth.
Current public [authentication documentation](https://docs.databricks.com/aws/en/oltp/projects/authentication)
states that new projects disable password connections by default and that the
built-in PgBouncer pooler does not support OAuth. No project-wide password
setting was changed, and broker compatibility is not claimed on OAuth-only
projects. Native OAuth now rejects the metadata-provided pooled hostnames.
`TestLakebaseConnectionTestsLive` and `TestLakebaseReplicaConnectionLive`
passed with `-race`; evidence is in `.local-test-results/native-ui-replica-live.log`.
Focused race regressions cover pinning, TLS hostname retention, authorization
failure, endpoint validation, and ordinary-password preservation on unmapped
connections (`native-ui-replica-race.log`). Pinned lint passed with zero issues
under matching Go1.26.1 (`native-ui-replica-lint.log`).

The action-provider allowlists in `portableActionProviders()` and
`guardProviders()` intentionally remain unchanged and omit `lakebase`.
The separate `lakebaseOwnedActionProviders()` list explicitly enables only
`analyze_table`, `vacuum_table`, `create_index_concurrently`,
`revert_created_index`, `reindex_concurrently`, `create_statistics`,
`revert_created_statistics`, and `alter_database_guc` after native validation.
Policy, ownership, trust, maintenance, replica and rollback/verification guards
still apply; App attachment alone does not establish each action's permissions.
Other provider-gated mutation families, including agent governance, remain
`provider_unsupported` even with operator approval. The deployed App principal
also lacks `CREATEROLE`, so governance is posture-only. Managed-instance changes
remain explicitly unsupported. See the current native and App acceptance
reports for the tested scope and identity-specific grants.
