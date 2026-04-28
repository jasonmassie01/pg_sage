# Changelog

## v1 (2026-04-28) — Autonomous DBA Release

> Published on GitHub `master` as tag `v1`. This release includes the
> autonomous DBA feature slices, the prior verification bug pass tracked in
> `docs/codex-bug-log.md`, the final provider-readiness contract updates, and
> the admin/legacy workflow verification pass.

### Functionality Change

This release moves pg_sage further away from a passive observability dashboard and closer to an autonomous DBA workflow. Findings, incidents, migration risks, proposed actions, approval state, and execution history now behave more like a single operational case queue that a DBA can triage end to end. The goal is for pg_sage to preserve the full story around a database problem: what was detected, why it matters, what the system recommends, what action was proposed, whether a human approved or rejected it, and what happened after execution.

The action lifecycle is now more deterministic and safer under real operating conditions. Proposed actions are filtered when their source finding has already been resolved, successful executions close the loop back to the finding, rejected actions observe cooldowns, and stale or duplicate create-index work is suppressed before it can create noise or risk. This matters because autonomous database work cannot rely on optimistic UI state alone; every action needs fresh evidence, an auditable state transition, and guardrails that prevent the system from repeatedly proposing or executing work that has already been handled.

Fleet behavior was tightened across settings, detail lookups, action execution, incident resolution, and managed database editing. In multi-database mode, pg_sage now requires explicit database targeting for mutations and ambiguous detail reads, refreshes runtime state after managed database changes, and keeps selected-database configuration separate from global configuration. These changes are intentionally conservative: once a product can operate across a fleet, wrong-database reads or writes become one of the highest-risk failure modes, so v1 prioritizes precise scoping over convenience.

The release also adds more trust-building surface for teams evaluating autonomy. Provider readiness, shadow-mode reporting, durable LLM cooldown tracking, migration safety cases, and incident playbook actions give operators a clearer view of what pg_sage can do, what is blocking it, and what it would have done before automation is enabled. The reasoning behind these additions is adoption-oriented: DB teams are more likely to trust autonomous execution when the product can first prove avoided toil, expose its constraints, and show a reliable decision trail.

### Added

- Cases now consolidate schema-health, forecast, incident, and query-hint work into one DBA case surface. Legacy `#/schema-health`, `#/forecasts`, `#/incidents`, and `#/query-hints` routes now open Cases with the relevant source filter selected, while the Cases API projects active/broken query hints as case evidence alongside existing findings and incidents.
- Migration-safety cases now include a deterministic DDL preflight report and PR/CI-ready script output. Case and action detail surfaces show lock/rewrite risk, live-risk checks when available, generated migration SQL, rollback or forward-fix guidance, verification SQL, and PR metadata without directly executing high-risk DDL.
- Incident playbook automation now covers runaway queries, connection exhaustion, WAL/replication risk, and sequence exhaustion in addition to lock blockers and idle-in-transaction incidents. Low-risk playbooks emit read-only diagnostics, PID actions still require exact PID evidence and approval, and sequence capacity work is generated as a forward-fix migration script instead of direct execution.
- Vacuum, bloat, and freeze cases now project explicit autopilot candidates. Table-bloat findings can propose guarded `VACUUM`, IO-saturated bloat cases are blocked to script/review output, XID wraparound findings get freeze-blocker diagnostics, and per-table autovacuum tuning produces PR/CI-ready reloption scripts with verification SQL.
- Query tuning now extends beyond `pg_hint_plan` hints. Suggested rewrites become PR-ready query rewrite artifacts with semantic and plan verification, broken hints can be retired through a safe metadata action, and repeated per-role `work_mem` hints can be promoted to reviewed role-level configuration changes.
- Provider readiness now uses provider-specific capability adapters for self-managed Postgres, Cloud SQL, AlloyDB, RDS, and Aurora. The readiness matrix exposes extension enablement paths, log-access expectations, provider limitations, and the expanded action family support instead of treating every Postgres endpoint as operationally identical.
- DDL safety preflight now records live-risk checks for table size, active workload, pending locks, replica lag, and lock-timeout configuration. High-warning live preflight output blocks direct execution and keeps the action in reviewed PR/script mode.
- Incident and maintenance coverage now adds autovacuum-falling-behind, standby-conflict, blocked-vacuum, concurrent reindex, bloat-remediation planning, `CREATE STATISTICS`, and parameterized-query action families, each with typed contracts and verification plans.
- Provider readiness now exposes the expanded action-family detail in the UI, including `ddl_preflight` and standby-conflict diagnostics, so operators can see exactly which autonomous capabilities are blocked, observable, script-only, or executable for each provider.
- Admin and legacy workflows were rechecked for v1: Users, Notifications, Fleet add/edit/import surfaces, Database and Alerts empty states, command palette navigation, not-found handling, and legacy `#/findings`, `#/schema-health`, `#/query-hints`, `#/forecasts`, and `#/incidents` routes.

### Fixed

- Fleet incident, finding, and action detail endpoints could scan all pools by ID and return the first matching row, so duplicate IDs across databases could surface the wrong record. Detail endpoints now require an explicit `?database=` in multi-pool fleet mode, while still allowing implicit lookup when exactly one database is registered.
- Successful recommendation actions left source findings open, so the UI could continue to offer the same action. Successful action execution now marks linked findings resolved and stores the action log id.
- Rejected recommendations could immediately reappear as pending approvals. Approval mode now suppresses recently rejected finding/SQL pairs during the cooldown window.
- Pending approval counts did not update live. SSE now emits action events from both `sage.action_queue` and `sage.action_log`, and the layout subscribes to refresh pending counts.
- Recommendations could still show manual action controls while a matching approval was pending. The UI now hides duplicate manual action controls when pending approvals exist.
- Emergency Stop cancelled monitoring goroutines, and Resume could not restart them. Emergency Stop now gates actions without cancelling monitoring, so Resume restores operation.
- Several bodyless UI `POST` calls were rejected with HTTP 415 because they omitted `Content-Type: application/json`. Fixed emergency stop/resume, notification test-send, DB test, incident resolve, and token budget reset call sites.
- Managed database edits updated `sage.databases` but did not refresh the active fleet runtime instance. Updates now remove the old runtime name and register the updated record.
- Managed database updates against missing ids could report an internal readback failure instead of not found. Store updates now check affected rows and the API maps missing records to 404.
- Notification channel toggles could persist masked secrets returned by the list API, corrupting Slack webhooks, PagerDuty routing keys, or email passwords. Store updates now preserve existing real secrets when masked placeholders are replayed, and API masking covers `smtp_pass`.
- Notification channel/rule update and delete calls reported success for missing ids, and rule creation relied on database FK errors for missing channels. Notification stores now return not-found/validation errors deterministically and handlers map them to 404/400.
- Notification channel updates treated omitted `enabled`, `name`, or `config` fields as false/empty values, so partial clients could accidentally disable or invalidate channels. The update handler now preserves existing omitted fields.
- Admin users could demote themselves or demote the last admin, potentially leaving the system with no administrator. The API now forbids self admin-role changes and last-admin demotion; the Users UI disables those controls.
- Edit Database `Test Connection` tested the saved connection instead of unsaved form edits. The edit form now submits current fields, and the backend merges the stored password when the password field is left blank.
- Edit Database `Test Connection` with unsaved preview fields bypassed the SSRF host guard used by Add Database testing. Preview edit tests now resolve and block unsafe hosts through the same safety path.
- Editing a managed database could send `max_connections` as a string from the UI, failing JSON decode into the Go integer field. The form now coerces `max_connections` numerically like `port`.
- The edit-form SSRF guard initially blocked testing an already-managed local/private database host, breaking legitimate local fixture edits. Edit tests now allow the stored managed host while still blocking unsafe host changes.
- Settings always saved global config, even when a specific database was selected. Fleet status now exposes database ids, Settings maps the selected database name to id, and selected-database edits use `/api/v1/config/databases/{id}`.
- Global Settings exposed `execution_mode` as an editable field even though global saves discarded it. The global UI now marks execution mode as database-only, while selected-database Settings can edit and persist it.
- `GET /api/v1/config/databases/{id}` returned config for nonexistent ids and defaulted null `execution_mode` to `manual`. It now returns 404 for missing databases and uses the managed DB default `approval`.
- Saved per-database config overrides could not be reset from Settings. A per-database override delete endpoint now removes stored DB overrides, and the Settings reset button calls it for DB overrides.
- Per-database `trust.level` reset removed the stored override but did not update the running fleet executor/status. Reset now synchronizes the effective inherited trust level back into runtime state.
- Multi-key config saves could partially persist earlier keys before later validation failures returned 400. Config batch saves now prevalidate all keys before writing any overrides.
- LLM model discovery could write a masked `llm.api_key` back into global config and ignored selected database scope. Discovery now uses a non-persistent model-discovery request and skips masked keys.
- `/api/v1/explain` accepted viewer-role requests even though it can run database queries. The route now requires operator-or-admin access and has API coverage for valid and invalid explain requests.
- Parameterized explain requests interpolated caller-supplied parameter strings directly into `EXPLAIN EXECUTE`. Parameters are now emitted as escaped SQL literals, with NUL bytes rejected as invalid input.
- Incident resolution from the UI sent an empty JSON request despite the API requiring a JSON body, so resolving incidents could fail with HTTP 400. The UI now sends `{ "reason": "" }`.
- Incident confidence never rendered because the API returns `confidence` but the UI read `confidence_score`. The incident detail drawer now renders the API field.
- Fleet-wide Query Hints, Incidents, and Action History read only the primary database for all-database views, hiding secondary database rows. These handlers now merge all registered pools, include `database_name`, sort deterministically, and apply global limits where needed.
- Action details searched only the primary database. Detail lookup now searches all pools so secondary-database action history rows are reachable.
- Manual action execution accepted any valid SQL for any numeric finding id, including missing, resolved, or mismatched findings. Execution now verifies the finding is open/unresolved and the SQL matches the recommendation before running DDL.
- Pending action lists and counts could expose queue rows for findings already resolved by another action, leading users to approve stale actions that could not safely run. Pending action queries and approval now require the linked finding to still be open and actionable.
- Fleet suppress/unsuppress without `?database=` could mutate the first matching finding id by map-order scan across databases. Mutations now require a database when more than one instance exists and only allow implicit targeting for a single connected instance.
- Emergency stop/resume returned 200 with zero changes for unknown database names and ignored persistence errors from writing the stop flag. Handlers now return 404 for unknown databases and surface persistence failures instead of reporting success.
- Fleet Health chart used raw database aliases as Recharts `dataKey` values. Aliases containing dots could be interpreted as object paths and fail to render; the chart now uses safe internal series keys and display names separately.
- Schema Health applied status/severity/category filters to the table but not to the summary stats, so the summary could contradict the visible rows. Stats now use the same filters, and the summary label now describes matching findings rather than always saying open findings.
- Query Hints treated missing `after_cost` as zero due JavaScript null coercion, creating fake 100% cost improvements. Cost improvement and average improvement now require both before and after costs.
- Query Hints lifecycle states existed (`retired`, `broken`) but `/api/v1/query-hints` only returned active rows, making the UI Status column misleading and hiding revalidation outcomes. The endpoint now returns recent hints across statuses by default and supports `status=active|retired|broken` filtering.
- Schema Health could not filter the `safety` thematic category even though the backend schema linter emits it. The category dropdown now includes safety, and live coverage verifies expanded rows show impact, recommendation, and suggested SQL.
- Fleet Health chart series keys were safe for dotted aliases but still depended on unsorted response object order, so series key-to-database mapping could shift between refreshes. Series are now sorted deterministically, and live coverage verifies chart rendering from seeded `health_history` rows across all three local databases.
- Incidents accepted `low`, `medium`, and `high` risk values plus newer source values, but the UI only labeled the older `safe`, `moderate`, and `high_risk` variants. The detail drawer now labels all valid risk/source values.
- Viewer users were shown the Pending Approval tab and the Actions page fetched operator-only pending approvals, causing forbidden requests for read-only users. Pending-review UI and polling now render only for admin/operator roles, with live viewer/operator boundary coverage.
- Notification channel UI did not expose PagerDuty even though the backend supports it, and selecting unsupported types would fall through to the email form shape. The Channels form now exposes PagerDuty with a routing-key field and stable browser-test selectors for all channel types.
- Notification masked-secret preservation now has browser-level coverage: creating a Slack channel through the UI, receiving a masked webhook from the API, toggling the channel from the UI, and verifying the database still stores the original webhook rather than the masked placeholder.
- Existing managed database connection tests in YAML fleet mode tried to decrypt the `sage.databases.password_enc` placeholder even though runtime credentials live in the fleet instance config. The endpoint now uses fleet runtime credentials for fleet-registered rows and only reports 404 for actual missing database records.
- Managed Database edit-form connection testing now has browser-level coverage for unsaved field changes and empty password fallback, including a failing unsaved database name followed by a successful retry without saving.
- Managed Database API/UI edits did not round-trip `max_connections`; saving from the UI could overwrite a configured fleet limit with `0`. The API now returns and accepts `max_connections`, defaults absent values safely, and the UI includes the field.
- In local YAML fleet mode, saving a managed database edit could close the primary fleet pool, which is also captured for auth and catalog storage, causing subsequent logins/API calls to hang or fail. No-meta fleet edits now refresh runtime metadata in place without closing the primary pool, with browser coverage for saved edits updating runtime trust state.
- Background/live refetches set `loading=true` even when data was already rendered, unmounting tables and collapsing/detaching expanded finding action panels while a user was approving or rejecting queued work. `useAPI` now keeps existing data mounted during background refetches and only shows loading on initial load or URL changes.
- Local no-meta fleet mode exposed Add Database even though new database credentials cannot be encrypted without an `encryption_key`, causing a 500. The store now returns a validation error, and browser coverage verifies the UI surfaces the `encryption_key` requirement.
- Global Settings reset could not remove global overrides correctly because hot reload mutated the live config and the API no longer had an immutable YAML/default baseline to restore from. Config routes now keep a detached baseline snapshot, expose `DELETE /api/v1/config/global/{key}`, reload live config from baseline plus remaining overrides, and the Settings UI can reset global overrides.
- The full-surface fixture did not include a deterministic invalid-index case and only printed finding summaries. The fixture now creates a failed concurrent unique index, asserts expected analyzer categories, schema-lint rule IDs, and active query hints across all three local databases, and polls until the expected surface appears.
- The legacy serial walkthrough could not run against local Docker fixtures without hardcoded database credentials and failed strict-mode locators when multiple finding detail/suppress controls were visible. The walkthrough now supports fixture DB credential env vars, has an encrypted meta-db config for add/edit/delete flows, and scopes duplicate UI controls to the first visible target.
- The encrypted walkthrough fixture was still a manual setup sequence. `run_walkthrough_fixture.ps1` now provisions isolated meta/target databases, seeds the fixture schema, starts the encrypted sidecar, resets the admin password, runs the 124-check walkthrough, and can restore the standard full-surface sidecar afterward.
- The long walkthrough used Playwright serial mode, so one failed checkpoint skipped every later check and hid independent endpoint/UI regressions. The global serial mode is now removed; the wrapper still runs the file with one worker to preserve fixture ordering, but later checks continue after failures.
- Successful create-index approvals could be followed by a stale analyzer cycle reopening the same missing-index finding as a new open row. Finding upserts now suppress immediate reopen of recently action-resolved findings while still allowing genuinely unresolved issues to reappear after the grace window.
- Stale or parallel create-index approvals could execute after an equivalent index already existed, creating duplicate indexes with auto-generated names. Manual/approved create-index execution now checks for an existing valid covering index before running DDL and records the action as successful without creating another index.
- Action lifecycle verification only checked row presence in browser tests. The full-surface harness now verifies pending rows across all three local databases, approve execution, rejected queue removal, executed action logs, resolved finding links, and no post-refresh missing/duplicate-index noise.
- Failed `CREATE INDEX CONCURRENTLY` attempts could leave invalid autogenerated index stubs that blocked the next approval of the same recommendation. Create-index execution now drops invalid covering index blockers before retrying the DDL.
- Transient lock timeouts during approved `CREATE INDEX CONCURRENTLY` actions could fail the UI workflow and leave an invalid index stub. Create-index execution now retries lock-timeout failures and cleans invalid blockers between attempts.
- Mutating action browser specs selected the first available pending create/drop action, so parallel runs could contend for the same database/object depending on queue order. The specs now use disjoint deterministic database/action targets.
- The full-surface high-total-time fixture used a PL/pgSQL loop that pg_stat_statements counted as one call, making the `high_total_time` category intermittent. The workload now uses psql `\gexec` so the high-frequency query is recorded as thousands of real calls.
- Incident resolve mutations in a multi-database fleet could resolve the first matching incident UUID by pool order when the UI did not send a database target. Resolve now requires an explicit database when multiple pools are connected, the UI sends the row database, and duplicate incident UUID coverage verifies only the selected database is mutated.
- Incident listing for a selected fleet alias could drop rows whose stored `database_name` was the physical PostgreSQL database name, and the UI could then resolve against that physical name instead of the fleet alias. Incident list responses now include `fleet_database_name` for mutations, and selected-pool listing no longer filters rows by alias.
- Last-admin demote/delete checks were split from the mutation, so concurrent requests could both pass a stale admin count and leave the system without an admin. User role changes and deletes now run in a locked transaction with DB-backed concurrency coverage.

## v0.8.5 (2026-04-12) — Hint Lifecycle, Stale-Stats ANALYZE, Security Hardening, Full Test Sweep

> 91 files changed across the sidecar. 17,016 lines added. 3,480 Go tests + 44 Playwright e2e tests across 6 spec files. Zero skips, zero failures. This is the most thoroughly tested release pg_sage has shipped.

---

### New Features

#### F1 — Hint Revalidation Loop
Hints are no longer fire-and-forget. A new background cycle (`tuner.verify_after_apply`, default off for backward compatibility) continuously validates every active `pg_hint_plan` hint against the live query planner:

- **Staleness check** — retire hints whose query has fallen below `min_query_calls` or been pruned from `pg_stat_statements`.
- **TTL check** — retire hints older than `hint_retirement_days` (default 14) unconditionally.
- **Cost regression check** — cost-compare hinted vs unhinted plan. Keep if `hinted_cost ≤ 1.2× unhinted_cost`; roll back and mark broken when `hinted_cost > unhinted_cost / 0.8`.
- **Redundancy check** — detect directives that no longer affect the generated plan (e.g., an `IndexScan` hint on a table whose only access path is already an index scan).

New directive parser (`hint_parse.go`) supports `Set(work_mem)`, `IndexScan`, `BitmapScan`, `NestLoop`, `HashJoin`, `MergeJoin`, and `Parallel`. Revalidation EXPLAIN carries its own `statement_timeout` so it cannot starve the collector.

#### F2 — Stale-Stats Detection + Autonomous ANALYZE
pg_sage now detects tables with stale statistics by correlating three signals: row-estimate skew (actual vs planned rows diverging by 10×+), modification ratio since last ANALYZE exceeding 10%, and last ANALYZE age over 60 minutes. When all three converge, the executor issues `ANALYZE <schema>.<table>` with full safety controls:

- Per-table cooldown (60 min) that respects autovacuum's own `last_analyze` timestamp
- Size ceiling (10 GB) — larger tables emit advisory findings instead of executing
- Maintenance-window gating (1 GB+) — large tables only ANALYZE during configured windows
- Statement timeout (10 min) with automatic cooldown extension on timeout
- Fleet-wide concurrency cap (1 concurrent ANALYZE across all databases)

#### F3 — Role-Level work_mem Promotion
When multiple queries owned by the same database role accumulate `Set(work_mem)` hints (default threshold: 5), pg_sage detects the pattern and recommends `ALTER ROLE ... SET work_mem` instead — one role-level setting replacing N per-query hints. Excludes `NOLOGIN`, `SUPERUSER`, and reserved roles. Scoped by `pg_stat_statements.dbid` to prevent cross-database pollution in shared-cluster deployments.

#### F4 — Extension Drift Detector (Enhanced)
Tightened drift detection for `pg_stat_statements`, `pg_hint_plan`, `hypopg`, and `auto_explain`. Missing critical columns (`plan_time`, `wal_records`) now produce explicit remediation hints with version-specific upgrade instructions, replacing the previous generic warnings.

#### F5 — Config Tooltip Infrastructure
Every Tier 1 and most Tier 2 config fields now carry `doc`, `warning`, `mode`, `docs_url`, and `secret` struct tags. A new build tool (`cmd/gen_config_meta`) reflects over `config.DefaultConfig()` to emit a 167-field JSON metadata file consumed by the React frontend. The `ConfigTooltip` component (WCAG-compliant, portal-mounted, keyboard-accessible) surfaces this metadata as hover/focus tooltips on every config field in the Settings page — including doc text, warning callouts, mode badges (fleet-only / standalone-only), and secret indicators. A reflection-based drift test fails the build if metadata falls out of sync with the Go struct tags.

#### F6 — Hint-Index Coordination
New deferral logic prevents the tuner from installing `pg_hint_plan` hints while the optimizer has pending index recommendations for the same query. When an index recommendation is in flight, the tuner defers hint creation until the index is either applied or dismissed — preventing the scenario where a hint masks the benefit of a better index. Plan scanning (`planscan.go`) detects whether a pending index would affect the query's execution path before deciding to defer.

#### F7 — Token Budget Dashboard
New `TokenBudgetBanner` React component on the Dashboard and Settings pages shows real-time LLM token consumption against the configured daily budget. Visual indicator transitions from green → amber → red as usage approaches the cap. Reads from the existing `/api/v1/llm/token-usage` endpoint with no additional backend changes.

---

### Fleet & Wiring Fixes

- **Fleet database wiring** — `FleetMgr.PoolForDatabase("all")` no longer returns a nil pool when fleet mode has a single database configured. The `wireRouter` extraction refactor (`wire.go`) splits 5 concerns (standalone wiring, fleet wiring, router setup, middleware chain, graceful shutdown) into testable functions with dedicated mode-specific wiring tests.

---

### Security Hardening

Six targeted fixes addressing edge cases surfaced during adversarial review:

1. **PagerDuty retry amplification** — exponential backoff with jitter replaces unbounded retries on transient 5xx responses. Max 3 attempts, circuit breaker after consecutive failures.
2. **Rate limiter connection leak** — login rate limiter now properly releases semaphore on early returns, preventing goroutine accumulation under sustained brute-force.
3. **Silent zero on invalid port** — `strconv.Atoi` failures on user-supplied port strings now return explicit validation errors instead of silently defaulting to port 0.
4. **CORS origin clarification** — `config_apply.go` validates CORS origin against an allowlist; previously accepted any origin header when the allowlist was empty.
5. **SSRF fail-closed** — `test-connection` endpoint rejects private/loopback IP ranges before attempting the connection, preventing internal network scanning via the API.
6. **Multipart size limit** — file upload endpoints enforce a 10 MB ceiling to prevent memory exhaustion from oversized payloads.

---

### Test Sweep (P0–P5)

The most comprehensive test expansion in the project's history. Every priority tier addressed:

| Priority | Focus | Tests Added |
|----------|-------|-------------|
| P0 | Config consistency — verify every YAML key round-trips through `Load()` → `Save()` without loss or mutation | 47 tests |
| P1 | Wire extraction — `wireRouter` decomposition with mode-specific wiring tests (standalone, fleet, unknown) | 19 tests |
| P2 | LLM degradation — malformed JSON, markdown-wrapped responses, empty bodies, timeout, rate limiting across advisor/optimizer/tuner | 43 tests |
| P3 | Config defaults — every field with a non-zero default verified against `DefaultConfig()` | 131 assertions |
| P4 | API contract — endpoint existence, method enforcement, auth requirements, response shapes | 208 tests |
| P5 | Playwright e2e — Dashboard, Databases, Settings, Navigation, Token Budget, Tooltips | 44 tests across 6 specs |

Total test count: **3,480 Go test functions + 44 Playwright e2e tests**. All packages above 70% coverage threshold. Zero test modifications to force passage — every failure fixed in implementation code.

---

### Documentation

- **End-to-end walkthrough verification** — every doc page cross-referenced against the actual codebase. Six inaccuracies fixed:
  - `--database-url` → `--pg-url` (deployment.md, 4 occurrences)
  - `SAGE_GEMINI_API_KEY` → `SAGE_LLM_API_KEY` (deployment.md, 3 occurrences)
  - LLM endpoint suffix `/chat/completions` removed (SDK appends it)
  - `--meta-db` description corrected from "SQLite path" to "PostgreSQL URL for fleet mode"
  - `llm.token_budget` → `llm.token_budget_daily` with correct 500,000 default (security.md)
- **MCP reference removal** — MCP (Model Context Protocol) was documented across 7 files but never implemented in the Go sidecar. Removed all references: `sage.mcp_log` table from CI schema and sql-reference.md, MCP step from try-it-out.md, port 5433 from firewall notes, `mcp:` stanza from demo config, nav entry from mkdocs.yml. Zero functional code affected — this was purely phantom documentation.
- `config.example.yaml` updated with every new tuner and analyzer field.

---

### Stats

| Metric | Value |
|--------|-------|
| Commits since v0.8.4 | 5 |
| Files changed (sidecar) | 91 |
| Lines added | 17,016 |
| Lines removed | 2,295 |
| Go test functions | 3,480 |
| Playwright e2e tests | 44 (6 spec files) |
| Go packages tested | 26 |
| Test failures | 0 |
| Test skips | 0 |

## v0.8.4 (2026-04-07) — Security Hardening + Tuner Pipeline

### Security
- **RBAC**: Added `RequireRole` to legacy API routes (config, emergency-stop, resume, suppress/unsuppress, pending count). Viewers can no longer escalate trust to autonomous or halt the fleet.
- **SQL injection**: Block EXPLAIN injection via multi-statement rejection + `statement_timeout` + read-only transactions. New `sanitize.QuoteIdentifier` package for all catalog-derived identifiers. Fixed VACUUM, ALTER DATABASE, and tuner SQL construction.
- **Executor allowlists**: Whitelisted 30 safe `ALTER SYSTEM` GUCs. Restricted SELECT to `pg_terminate_backend`/`pg_cancel_backend`. Restricted `ALTER TABLE` to `SET`/`RESET`/`TABLESPACE`. Validate trust level strings. Cap concurrent DDL at 3. Auto-drop invalid indexes after failed `CREATE INDEX CONCURRENTLY`. Derive advisor `ActionRisk` from SQL type instead of hardcoding `safe`.
- **Auth**: Cookie `Secure` flag enabled. Password minimum 8 characters. Login rate limiting (5 attempts / 15 min per email). Self-deletion + last-admin protection.
- **API hardening**: Security headers middleware. Content-Type validation. SSRF protection on `test-connection`. Error messages sanitized (no more `err.Error()` leaked to clients). `X-Forwarded-For` only trusted from loopback. Notification channel secrets masked.
- **Secrets**: Removed hardcoded Gemini API key from `docker-compose.test.yml`. Admin password printed to stderr only. `create_admin` uses environment variables. `DeriveKey` upgraded to argon2id with v1 backward compatibility. Demo config uses env var.

### Tuner Pipeline
- `BuildInsertSQL`/`BuildDeleteSQL` now use `norm_query_string` column (was `query_id`, which doesn't exist in `hint_plan.hints`).
- Executor allowlist updated for `INSERT`/`DELETE INTO hint_plan.hints`.

### Fleet Hardening
- Schema bootstrap advisory lock changed from non-blocking `pg_try_advisory_lock` to blocking `pg_advisory_lock` with 30s timeout.
- `CREATE SCHEMA` now uses `IF NOT EXISTS`. All `DROP SCHEMA` operations in tests wrapped in advisory lock.
- `autoexplain.ConfigureSession` tolerates permission denied (SQLSTATE 42501) on managed DBs where role-level defaults suffice. Coverage boost to 83%.

### API
- New LLM token usage endpoint.
- Config apply/audit handlers.
- Settings page UI improvements.
- New `create_admin` CLI command.

### Test Infrastructure
- `auth`, `notify`, and `store` tests now hold `pg_sage` advisory lock for test duration to prevent schema drops mid-test.
- Store config tests re-insert FK user before each test.
- All 22 packages pass with `-p 4` (zero failures, zero skips).

## v0.8.3 (2026-04-04) — Cloud E2E + LLM Token Optimization
- Cloud E2E validation across 8 managed PostgreSQL databases:
  RDS PG14/18, Aurora PG14/17, Cloud SQL PG14/18, AlloyDB PG14/17
- Auto-detect cloud environment (rds, aurora, cloud-sql, alloydb)
- ALTER SYSTEM → ALTER DATABASE rewriting for managed platforms
- Executor max-retry limit (3 failures → mark as acted_on)
- LLM deduplication: skip redundant calls when open findings/hints exist
  (optimizer, tuner, advisor all check sage.findings/sage.query_hints first)
- 11 token waste fixes: bloat category mismatch, vacuum validation bug,
  thinking-model budget, CapturePlans loop hoist, column stats filtering,
  per-cycle table cap, per-query rewrite dedup, briefing LIMIT,
  retry scope (429/503 only), tuner stats cap, single-symptom deterministic skip
- Thinking model support: +16384 token overhead for Gemini 2.5 reasoning
- Cross-platform findings: 1615 total, 373 open, 802 acted on across 8 DBs
- All packages above 70% test coverage

## v0.8.2 (2026-04-03) — LLM Tuner + Query Rewrites
- Query tuner: hybrid deterministic rules (7 symptom kinds) + LLM-enhanced hints
- LLM-powered query rewrite suggestions alongside pg_hint_plan directives
- Rewrite suggestions surfaced in dashboard with rationale
- Alert notification (`query_rewrite_suggested` event) when rewrite is suggested
- Index optimizer multi-query consolidation (8 queries → minimal index set)
- E2E test suite: 54 subtests against real Gemini API
- 771+ tests

## v0.8.1 (2026-03-27) — Patch
- Add `google_ml` to all schema exclusion lists (Cloud SQL compatibility)
- Bump default LLM max_tokens to 8192 (Gemini 2.5 Flash thinking token fix)
- Add retry loop to index validity post-check (catalog propagation delay)
- Prevent re-execution of already-acted findings (re-drop race fix)
- Executor cooldown for recently created indexes
- Verified on Cloud SQL PG16/17 and AlloyDB PG17
- 588 tests, 0 failures

## v0.8.0 (2026-03-26) — Fleet Mode + Dashboard
- Fleet manager: single sidecar → N databases via `mode: fleet` config
- `DatabaseManager` with per-database collector/analyzer/executor goroutines
- Per-database advisory locks, trust levels, executor toggles
- Per-database LLM token budget (equal, proportional, or priority-weighted)
- Database-aware data model (every finding, action, metric carries `database_name`)
- Prometheus labels: `{database="prod-orders"}`
- Graceful per-database failure (one DB down doesn't crash others)
- REST API: 14 endpoints on `:8080` alongside MCP
- Fleet overview: `GET /api/v1/databases` with health scores
- Findings, actions, snapshots, config — all filterable by `?database=`
- Config hot-reload via `PUT /api/v1/config`
- Emergency stop/resume per-database and fleet-wide
- Web dashboard (React SPA embedded in binary via `//go:embed`)
- Demo environment: Docker Compose with 7 pre-planted problems, 46 verification checks
- 584+ tests, 0 failures, CI green (6 workflows)

**Integration bug fixes shipped in v0.8.0:**
- VACUUM routed through non-transaction connection (pgxpool wraps in tx by default)
- Trust ramp `ramp_start` config honored on first boot (was always `now()`)
- Unused index window default changed to 7 days (was 0, caused index churn)
- Advisor strips markdown fences from Gemini JSON responses
- `database_name` resolved to actual instance name (was showing "all")

## v0.7.0 (2026-03-26)

### Go Sidecar — The Product

pg_sage is now a Go sidecar binary that connects to any PostgreSQL 14-17 database.
The C extension is frozen at v0.6.0-rc3 (security fixes only).

#### Features
- **Standalone mode** — single binary, no extension install required
- **Index Optimizer v2** — LLM-powered index recommendations with HypoPG validation, confidence scoring, per-table circuit breakers, 8 validators
- **Vacuum Tuning** — per-table autovacuum analysis via LLM
- **WAL/Checkpoint Tuning** — max_wal_size, wal_compression, checkpoint analysis
- **Connection Pool Analysis** — max_connections, idle timeout, pooler detection
- **Memory Tuning** — shared_buffers, work_mem, cache hit ratio, spill detection
- **Query Rewrite Suggestions** — N+1, correlated subquery, OFFSET pagination detection
- **Bloat Remediation Planning** — VACUUM FULL vs pg_repack vs do nothing
- **MCP Server** — Claude Desktop and AI agent interface
- **Prometheus Metrics** — full observability endpoint
- **Dual-Model LLM** — separate models for general tasks vs index optimization
- **Trust-Ramped Executor** — observation -> advisory -> autonomous with rollback

#### Verified Platforms
- Google Cloud SQL (PG14, PG15, PG16, PG17)
- Google AlloyDB (PG17)
- Self-managed PostgreSQL (PG14-17)
- Amazon Aurora — test plan ready
- Amazon RDS — test plan ready

#### Testing
- 530 tests across 14 packages, 0 failures
- Live integration testing on Cloud SQL PG16, PG17, and AlloyDB PG17
- E2e tests with Gemini: 3 real LLM findings verified

#### C Extension (Frozen)
- v0.6.0-rc3 — no new features
- Works on self-managed PostgreSQL with auto-explain hooks
- SQL functions: sage.explain(), sage.diagnose(), sage.briefing()
