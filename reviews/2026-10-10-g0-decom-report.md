# G0 workstream `decom`: decommission AgentDB provisioning

Branch `claude/g0-decom`. Spec: `reviews/2026-10-05-agentdb/AGENTDB-SPEC.md` §0, §2.5, §6.12,
§8.3, §9, §10.1 (G0-01, G0-02, G0-07), §12; `baseline-refresh.md` (fleetlearn.Boundary).

## What was built

- `sidecar/internal/decommission/` (new, allowlisted): read-only inventory (`Build`), per-provider
  delete templates (golden), credentials list (names only), acknowledgement store and handlers,
  startup report, retired-config checks, `RemovedRoutes`, `LegacyTables`, operator runbook
  (`README.md`). Fixture: `testdata/legacy_schema.sql` (the v2.3.1 AgentDB schema).
- `internal/schema/decommission_migration.go`: `sage.agentdb_decommission`; deletes persisted
  `agentdb.*` overrides with one `sage.config_audit` row each (waits until the audit table exists).
- `internal/config/retired_sections.go`: a registry of retired top-level sections, stripped before
  the strict decode; the decommission package registers `agentdb` and `agentdb_decommission`.
- `internal/api/decommission_routes.go`: admin-only GET inventory, POST ack.
- `cmd/pg_sage_sidecar/decommission_wiring.go`: startup report on the control pool.
- Removed: `internal/agentdb`, `internal/api/agent_db_*`, auth exemptions, `cmd/.../agentdb_*`,
  retention agent rules (and the now-dead `optional` purge flag), `fleetlearn.Boundary`'s agentdb
  arm and `Isolated` (Boundary now takes only tags), AgentDB config and keys, the web pages, nav
  entry and Playwright specs; bundle and generated docs rebuilt. Docs archived to
  `reviews/archive/agentdb-2026-05/`; README, index, neon-supabase, reverse spec and the wave-0
  contract point to the runbook.

## Decisions

- Operators run the inventory as the spec says: a startup report plus the admin API; no CLI.
- Selection widens "an execute_live attempt" to every live attempt kind (`*_live`,
  `live_reconcile_status`). Over-reporting is safer than forgetting a billed resource.
- RDS final snapshot items are listed for every kept (non-disposable) RDS instance with evidence,
  by name prefix: the snapshot name carries the destroy time, which was never recorded.
- Local artifacts are listed only where `credential_scope` proves pg_sage ran the CREATE, so a row
  that merely names an existing database never gets a DROP template.
- A config ack with unknown ids records the known ones and warns; the API refuses all-or-nothing.
- A failed startup inventory logs an error and does not block start.
- Retention rules for the legacy tables were removed, so evidence rows are no longer purged.
- Kept (ambiguous or out of scope): `internal/clone`, `internal/migration`, and the reconciler
  ideas are not in code any more (the reconciler lived in `internal/agentdb`, now deleted); the
  spec's narrowed broker is a G2 rebuild from `hosted_*.go`/`lakebase_runner.go` at `62d27f6e`.
  The `tuning` test named `agent_db_test.go` was about the tuning agent and was renamed.

## Checks

- G0-01: PASS — `TestBuild_SelectsTheLegacyEstateByEvidence`, `TestSelect_*`,
  `TestDeleteTemplates_MatchGolden`, `TestBuild_OlderSchemaVersions` (PG14, 17, 18).
- G0-02: PASS — `TestNoLegacyReferencesOutsideTheAllowlist` (runs where git can read the work
  tree; skips in the Docker run because this worktree's `.git` points at a Windows path, passed on
  the host), `TestRemovedProvisioningRoutes_Return404AndNeedAuth`,
  `TestRemovedProvisioningRoutes_AnonymousIsUnauthorized`.
- G0-07: PASS — `TestLegacySection_*`, `TestConfigLoad_*EndToEnd`,
  `TestDecommissionMigration_DeletesPersistedOverridesWithAudit`.

## Test Results

**Command:** `go test -p 2 -count=1 -cover -timeout 2400s ./...` (PG17, Docker golang:1.25)
**Total:** 107 packages ok, 7 failed, 0 skipped at package level; one test skip (above).
Failures, all outside this diff and passing on rerun or known local-only:
`cmd TestMetaReconcileConcurrentPassesPublishOneRuntime` (deadline under load; passes alone),
`executor TestExecuteFindingAppliesDecisionLockCeiling` (passes alone), `policy
TestLeaseQueueCancelledWaitLeavesQueue` (passes alone), `sre
TestDurability_OutageBlocksHandoffUntilVerified` (5 s ping deadline; passes on rerun),
`sre/probes TestCatalog_StatStatementsStaysFast...` (passes alone), `mcp
TestToolReferenceDocsMatchSchemas` (known CRLF), `sre-bench TestReplayCorpus` (180 s timeout).
PG14 and PG18 on decommission, schema, retention, store, config, fleetlearn, api: all ok.
`-race` on decommission, config, fleetlearn: ok. Lint: 0 issues. Web: lint ok, 585 tests pass,
build ok. E2E (`-tags=e2e ./e2e/`): ok, 22 pass, 7 skips (live LLM, allowlisted).

**Coverage:** decommission 92.1%, config 92.9%, schema 84.5%, retention 87.1%, store 76.1%,
api 79.6%, fleetlearn 89.0%, fleet 84.7%, tuning 91.3%, cmd/pg_sage_sidecar 79.5%. All packages
meet coverage thresholds.

**Mutation checks:** dropping `create_operation_id` evidence, and dropping the
`credential_scope` guard on local DROP templates, each fail `TestSelect_*`.

## Left

- G1: drop the 27 tables by `decommission.LegacyTables` (§12 step 5).
- The archived files still contain the AU-10 credentials; the `hygiene` workstream scrubs them.
