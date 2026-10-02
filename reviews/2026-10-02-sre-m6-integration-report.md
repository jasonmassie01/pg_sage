# Sage SRE M6 integration report (reactive + runways + runbooks)

Date: 2026-10-02. Branch `claude/sre-m6` (worktree `pg_sage-m6`), from `claude/sre-m5`
(M3 + M4 + M5 actions + M5 SLO). Not pushed. Test database: own PG17 `pgsage-ag3`
(:55473); shared matrix PG14 (:55414) and PG18 (:55418).

## What was merged, in order

| Commit | What |
|---|---|
| `7622865` | merge `claude/sre-m6-reactive` (checkpoint storm, temp files, replication lag, LWLock) |
| `c86e1f0` | test fix: `TestSRESignals_PagePolicy` assumed `automatic_start` defaults to false (superseded by the base's identical fix, below) |
| `c6d6e90` | merge `claude/sre-m6-runways` (wraparound, disk/WAL, sequence runways; runway monitor; disk-full credit) |
| `9f56389`, `88c1de7` | test + fix: the M6 probes that read other sessions declare `pg_read_all_stats` (see "Semantic fixes") |
| `67453ad` | merge the updated base `claude/sre-m5` (`d2efb25`, the page-policy test fix; the base's wording kept) |
| `33389ba` | merge `claude/sre-m6-runbooks` (signed runbooks, incident memory, REST/MCP/web) |
| `41b1614` | test: one event-type check admits every event type; every plan carries both signals under the probe ceiling |
| `7876965`, `d0469af` | test + docs: `forecaster.disk_capacity_bytes` 0 means undeclared |
| `307057a` | test: merged graph node test back under 50 lines |
| `d99e0fc`, `3e4656b` | test + ci: PGIncidentBench split into family shards |
| `5815a2a` | build(web): dist rebuilt after the config doc change |
| `09d2055`, `23fea81` | CHANGELOG bullets; permissions page lists the M6 probes' privileges |

No rebase, no force-push. Every merge was followed by a conflict-marker scan
(`git grep -n '^<<<<<<< \|^>>>>>>> '`: none), `go build ./...`, `go vet ./...` and the touched
packages' tests on PG17.

## Conflicts and how each was resolved (both sides kept)

### Merge 1: reactive (14 files)

- **`causal/graph.go`, `graph_test.go`**: M5 had `v3Nodes` (recent_change), reactive added
  `m6Nodes`. One graph: v1 + v2 + v3Nodes + m6Nodes; node count 15 + 18 = 33.
- **`sre/plan.go` `diagnose`**: kept M5's `slo_burn` case and reactive's `default: diagnoseM6`.
- **`sre/request.go`**: `TriggerSLOBurn` and the four M6 trigger kinds, all in `triggerKinds`.
- **`probes/catalog_capacity.go`**: `replication_lag` keeps M4's `needsStats` (pg_read_all_stats)
  and gets reactive's `Version = "v2"`.
- **`probes/registry.go`**: validate M4's required roles, then the variants, then reactive's
  extension token rule (`validateExtension`).
- **`probes/runner.go`**: M4's `checkRoles`, then reactive's `resolveExtension`, in the same
  read-only transaction.
- **`cmd/.../database_runtime_sre.go`**: reactive's combined trigger source (RCA + detector)
  plus M5's `Signals`.
- **`config/sre.go` + `docs/configuration.md`**: `sre.automatic_start` default `true` (master,
  M4) with reactive's mention of the detector; doc tag kept under 200 characters and still
  containing "false" and "Default: true" (`TestSREAutoStart_DocTagSaysOnByDefault`,
  CHECK-T16). `config_meta.json` and `config-lifecycles.md` regenerated with `gen_config_meta`.
- **`sre-bench/bench_test.go`, `README.md`**: M5's `benchBudget` (replay + live-model budgets)
  with reactive's larger pass budget; README documents `SAGE_BENCH_FAMILIES`.
- **CHANGELOG**: union.

### Merge 2: runways (22 files)

- **Graph node list name collision**: runways named its list `v3Nodes`, the same identifier
  as M5's recent_change list. Renamed to `runwayNodes` and the file `graph_v3.go` to
  `graph_runway.go`. Graph = v1 + v2 + recent_change + reactive + runways = 44 nodes; the
  families test lists all ten families.
- **Plan selection (`worker.go`, `runway.go`, `signals.go`)**: M5 built the plan with
  `planWithSignals(kind, ...)` (calling `planFor`), runways with `c.plan(kind)` (runway or
  family plan). Merged so `c.plan` builds the family or runway plan and then adds M5's signal
  probes (`addSignals`, was `planWithSignals`). Runway investigations therefore also collect
  the change feed (a deploy can explain disk growth). Largest plan: disk/WAL, 9 probes, 11
  with both signals, under the 12-probe ceiling (pinned by a new test).
- **`plan.go`**: `diagnose` keeps `slo_burn`, the three runway kinds and the M6 default;
  `seriesProbes` holds the M5/reactive/runway series.
- **`coordinator.go`, `record.go`, `conclusion.go`**: `Signals` + `Advisor` deps;
  `Summary.CustomerImpact` + `Summary.Proposals`; conclusion validates both.
- **Runtime wiring** (`database_runtime.go`, `database_runtime_sre.go`): M5 signals, M5
  actions start, combined triggers, runway advisor, `RunwayWindow`, runways start.
- **Config**: `SREConfig` has SLO, ChangeEvents, Actions and Runways, all defaulted and
  validated; runway keys are YAML-only beside the M5 keys in `config_consistency_test.go`.
- **`schema/bootstrap.go`**: `ddlRunwaySamples` after M5's `ddlSRESLOChangeEvents,
  ddlSREActions`.
- **Probe families** (`registry.go`, `registry_test.go`): TempFiles, Waits (reactive) and
  Sequences, Runway (runways).
- **Bench**: per-scenario budget (runways) over the selected scenarios
  (`SAGE_BENCH_FAMILIES`, reactive) plus M5's replay and live-model budgets: 30 s per
  selected scenario and repeat + 3 min (+60 min with a live model). `Derive` rules,
  `Scenarios()`, scenario-family test and README cover all families.
- **Test helper collision**: `restrictedPool` existed in both M4's `visibility_db_test.go` and
  runways' `catalog_runway_db_test.go` (different signatures); runways' is now
  `runwayRestrictedPool`.
- **CHANGELOG, docs/configuration.md**: union (approved actions and runways sections).

### Merge 3: runbooks (13 files + dist)

- **`worker.go`**: runbooks inserted `applyRunbook` between diagnosis and model turn and
  passed the run to `conclude`; runways passed custodian proposals. `conclude` now takes an
  `attachments` value (`runbook`, `proposals`) applied to the summary and to the
  deterministic fallback (which also keeps the model's memory). Proposals are computed on the
  final diagnosis, as on the runways branch.
- **`record.go`, `conclusion.go`**: Summary has CustomerImpact, Proposals, Runbook, Memory;
  all four are validated.
- **API** (`sre_handlers.go`): M4 start/stop/resume, M5 action routes, runbook routes.
- **MCP** (`server.go`): intent, SRE, M5 action, M5 signal and runbook tools. `operatorCtx` was
  declared identically by the M5 action tests and the runbook tests; one kept.
- **Schema**: bootstrap ends `ddlRecommendationAll, ddlRunwaySamples, ddlSRERunbooks`;
  `sreTables` (pre-M1 upgrade simulation) has the M5 SLO/action tables and the four runbook
  tables, parents before children.
- **Web**: SLOs page + Runbooks page routes and nav; the Cases panel shows action proposals,
  the runbook run and similar incidents.
- **`internal/api/dist`**: conflicting bundles discarded; `npm ci`, `npx vitest run`,
  `npm run build` from the merged source; rebuilt again after the last config doc change.
  `index.html` references `assets/index-4dbpCeNn.js` and `assets/index-CTgYmBWX.css`, the
  only two files in `dist/assets`; no conflict markers.

### Base update

`67453ad` merges `claude/sre-m5` at `d2efb25` (coordinator's fix of
`TestSRESignals_PagePolicy`); conflict with my identical fix resolved to the base's version.

## Semantic fixes found during integration

1. **[BUG, fixed] M6 probes could read a missing privilege as "healthy".** M4 (CHECK-08) made
   every probe that reads other sessions declare `pg_read_all_stats`, so a role without it
   gets `no_privilege` instead of a healthy zero. The M6 branches predate that. Five M6
   probes read other sessions: `lwlock_waits` (wait events), `standby_replay_state` (longest
   standby query), `temp_spill_statements` (pg_stat_statements hides other roles' query ids),
   `xid_runway` (busy autovacuum workers) and `xmin_horizon` (xmin holders). Without the role
   the LWLock and autovacuum-saturation mechanisms would have been ruled out on a missing
   privilege. They now declare it (`needsStats`), the M4 visibility tests list them, and the
   permissions page and CHANGELOG say so. `temp_file_holders` and `wal_directory` already fail
   honestly (`pg_ls_tmpdir`/`pg_ls_waldir` need `pg_monitor`) and are unchanged.
2. **[BUG, fixed] `TestSRESignals_PagePolicy`** assumed `automatic_start` defaults to false
   (M5-slo predates master's default change). Fixed on the base by the coordinator too.
3. **[DOC BUG, fixed] `forecaster.disk_capacity_bytes`** said "0 = auto-detect". No cheap
   honest detection exists (SQL cannot report free space; the sidecar may run on another
   host), so the tag, docs and CHANGELOG now say 0 = undeclared: no disk runway and no
   disk-full credit (managed services are never credited). A config test pins it.

## Causal graph: one version

Released: `v1.7.0` and `origin/master` ship `causal-v2`; nothing released uses v3. The merged
graph is **`causal-v3`**, 44 nodes in 12 families:

- v1 (6): `idle_in_tx_holder`, `prepared_xact_holder`, `ddl_lock_queue`, `hot_row_contention`,
  `plan_flip_regression`, `same_plan_latency_regression`
- v2 (8): `pool_fan_out`, `blocked_backlog`, `connection_leak`, `inactive_slot`,
  `slow_consumer`, `archiver_failure`, `write_surge`, `sage_own_action`
- v3, M5 (1): `recent_change`
- v3, M6 reactive (18): `max_wal_size_undersized`, `forced_checkpoints`,
  `short_checkpoint_timeout`, `checkpoint_write_burst`, `runaway_spill_query`,
  `repeated_spill_statement`, `work_mem_undersized`, `wal_send_backlog`,
  `standby_flush_backlog`, `standby_replay_backlog`, `replay_paused`, `standby_query_delay`,
  `replication_write_surge`, `lock_manager_contention`, `subtrans_slru_contention`,
  `multixact_slru_contention`, `wal_write_contention`, `buffer_contention`
- v3, M6 runways (11): `xmin_held_by_session`, `xmin_held_by_prepared_xact`,
  `xmin_held_by_replication`, `autovacuum_saturated`, `autovacuum_disabled`,
  `autovacuum_cancelled`, `xid_consumption_surge`, `database_growth`, `sequence_type_limit`,
  `column_narrower_than_sequence`, `explicit_maxvalue_limit`

No code branches on the version string. The three branch tests asserting `causal-v3`
(`signals_test.go`, `graph_m6_test.go`, `graph_v3_test.go`), `coordinator_m6_db_test.go` and
`model_runway_db_test.go` agree; `store_case_db_test.go` keeps `causal-v2` as historical
stored rows, which still resolve (every v1/v2 node is kept).

## Event-type and other shared CHECK constraints

- Neither M6 branch adds an event type (runways keep proposals in the summary, runbooks keep
  runs in `sre_runbook_runs`). The final `sre_events_event_type_m3` is M5's union rebuild
  (`ddlSREActionEventTypes`), which runs after M3's `ddlSREModelEvents` in bootstrap.
- New test `schema/sre_event_types_union_test.go` reads the event types from the Go source
  (`Event*` string constants in `internal/sre` and the `typ` literals of
  `internal/sre/action`: 20 types) and asserts there is exactly one event-type check and it
  admits all of them, after a fresh bootstrap and after an upgrade from the M2 inline check
  (bootstrapped twice). Evidence: allowed = `action_decided, action_executed, action_failed,
  action_proposed, action_recheck, action_refused, action_requested, claimed, concluded,
  created, evidence_purged, model_disagreed, model_rejected, model_reviewed, pinned,
  recovery_sample, recovery_verdict, step, transition, unpinned`. Mutation: adding
  `EventMutant = "mutant_type"` without a migration fails both tests.
- Trigger kinds and families have no CHECK constraint (`length(family) BETWEEN 1 AND 64`);
  the runbook vocabulary reads `triggerKinds`, so the merged list feeds it. The value
  ledger's `kind IN ('xid_wraparound','disk_full_slot','lock_storm')` already admits the
  runway credit.

## Shared registries (verified after the last merge)

Probe catalog and families; `seriesProbes`; trigger kinds; `diagnose`; bench `Scenarios()`
(11 families, 75 scenarios), `Derive` rules, scenario-family test; retention declarations
(`runway_samples`, M5 SLO/change tables, runbook tables; `TestRetentionRules_*` pass);
`sreTables`; YAML-only keys (`sre.slo.*`, `sre.change_events.*`, `sre.actions.*`,
`sre.runways.*`; `TestConfigConsistency_*` pass); MCP tool list; API routes; web routes/nav;
CHANGELOG (every bullet of the base and the three branches is present, checked line by line).

## CI bench split

`.github/workflows/ci.yml`: the single 1800 s bench step became three steps in the PG17
`test` job and in every `integration-matrix` leg (14, 15, 16, 18), selected by
`SAGE_BENCH_FAMILIES` (verified working: unknown family fails, the shards run only their
families):

| Shard | Families | Scenarios | PG17 runtime here (both live arms) |
|---|---|---|---|
| core | lock_blocking, connection_pressure, wal_retention, plan_regression | 31 | 129 s test (143 s wall) |
| M6 reactive | checkpoint_storm, temp_file_explosion, replication_lag, lwlock_contention | 22 | 398 s (439 s wall) |
| M6 runways | wraparound_runway, disk_wal_runway, sequence_runway | 22 | 218 s (241 s wall) |
| all families, one run | all 11 | 75 (74 scored) | 724 s test (794 s wall) |

Each step: `SAGE_BENCH_RUN=1`, `go test ... -timeout 2400s`, `timeout-minutes: 40`, and in
the PG17 job its own `SAGE_BENCH_REPORT_DIR` under the uploaded `pgincidentbench/` artifact.
The first shard runs only when the earlier steps passed (as before); the later shards run
even when an earlier shard failed, but not when the bench was skipped. **Matrix choice:** no
reduction; every leg runs all three shards, because the M6 probes have per-version variants
(`pg_stat_bgwriter` vs `pg_stat_checkpointer`/`pg_stat_io`, `pg_stat_wal` columns removed in
18). Each shard is well under 20 minutes here; GitHub runners are slower, so the 40-minute
limits leave room. `sre-bench/ci_split_test.go` enforces the contract (disjoint shards that
cover every bench family, gating, 2400 s, step timeout 40-45, per-shard report dir, and each
shard's own bench budget under 2400 s); mutation (dropping `sequence_runway` from a shard)
fails it. `.skip-allowlist` comment updated; `internal/startup` CI-contract tests and
`skipbudget` tests pass.

## Test Results

**Command:** `go test -count=1 -cover -timeout 60m -json ./...` (Docker `golang:1.25`, repo
root mounted, PG17 `pgsage-ag3`, `GEMINI_API_KEY` empty), then reruns of the failed
packages; matrix `go test -p 4 -count=1 -cover` of the touched packages on PG14 and PG18;
`go test -race -count=1 -cover ./internal/sre/... ./sre-bench/` on PG17; the bench as above.
**Total (full suite, PG17):** 9857 passed, 5 failed, 13 skipped (76 packages: 71 ok, 5 failed
on the first pass; all 5 pass on rerun, see Failures).
**Coverage (touched packages, PG17):**

| Package | Coverage | Package | Coverage |
|---|---|---|---|
| internal/sre | 86.7% (87.0% rerun) | internal/api | 76.5% |
| internal/sre/action | 82.4% | internal/mcp | 79.7% |
| internal/sre/causal | 94.4% | internal/schema | 81.6% |
| internal/sre/changefeed | 87.7% (90.0% rerun) | internal/retention | 100.0% |
| internal/sre/probes | 92.4% | internal/store | 74.5% |
| internal/sre/runbook | 94.4% | internal/runway | 89.1% |
| internal/sre/signed | 100.0% | internal/executor | 82.9% |
| internal/sre/slo | 88.9% | internal/value | 95.3% |
| internal/config | 89.5% | internal/autonomy | 79.6% |
| cmd/pg_sage_sidecar | 71.7% | cmd/gen_config_meta | 86.4% |
| internal/startup | 92.2% | sre-bench | 60.1% without the bench, 81.2% with it |

**PG14 / PG18 (touched packages, 22 packages):** PG14 4364 passed, 1 failed, 1 skipped; PG18
4363 passed, 2 failed, 1 skipped. Coverage within 2.4 points of PG17 on both (e.g. sre 86.9%,
probes 92.4/92.5%, causal 94.4%, retention 100/97.6%).
**Race (PG17):** `internal/sre/...` and `sre-bench` 994 passed, 0 failed, no `DATA RACE`.
**Lint:** `golangci-lint run ./...` 0 issues. **Web:** `npx vitest run` 45 files / 237 tests
passed; `npx eslint src` clean; `npm run build` ok.

### Skipped Tests (must be zero or justified)
13 in the full suite, all pre-existing and allow-listed: 8 live-cloud tests (AWS RDS, Cloud
SQL, Lakebase, agentdb gauntlet x3, Azure x2: `PG_SAGE_LIVE_*` not set), 2 live-LLM tests
(`PG_SAGE_LIVE_LLM` not set by the rules), `TestRCAChildProcessFixture` (subprocess helper),
`TestResolveLogDir_AbsoluteWindows` (Linux container), `TestPGIncidentBench` (gated by
`SAGE_BENCH_RUN`; run separately below). Inside the bench, `wal-archiver-failure` is skipped
(`archive_mode` off), as before.

### Failures
- Full suite, first pass (PG17): `TestPoller_SnapshotChanges`, `TestModelProbe_ProbeCeilingHonored`
  and `TestFleetServiceHonorsTimeWindowPerSource` failed with `connection refused` / dial
  timeouts to `host.docker.internal:55473` while the PG14 run and other agents' suites
  loaded Docker's port proxy (PostgreSQL did not restart); `TestRunner_OneProbePerDatabase`
  (peak concurrency sampled as 0) and `TestComposedSRE_M6_DetectorOpensAnLWLockInvestigation`
  (LWLock storm under load, inconclusive) are timing tests. Rerun of the five packages: all
  pass except `TestStore_StaleWorkerCannotCommit` (300 ms lease; fixed in PR #61, not on this
  base), which then passed 3/3 alone.
- PG14: `TestFleetServiceConcurrentReadersAgree` (internal/value, untouched by M6) failed once
  under load; package rerun ok, then 5/5.
- PG18: `TestService_ExportIsRedactedAndComplete` (bootstrap deadline under load; package
  rerun ok) and `TestComposedSRE_M6_DetectorOpensAnLWLockInvestigation` (failed in the loaded
  run and once more in a `-count=2` run; then 3/3 on PG18 and 4/4 on PG17; the reactive
  branch head also passed 3/3 on PG18). The test has a 90 s budget for a 24-session commit
  storm to show sustained WAL-write waiters; it is load-sensitive on the shared servers.
- **Bench, all families in one run (PG17): FAILED one gate.** `disk-database-growth-under-load`
  (disk/WAL runway, noise): the causal-graph arm named `write_surge` instead of
  `database_growth`, so CHECK-42-NOISE (disk_wal_runway) failed (noise 1/2 vs clean 3/3) and
  M3-LLM-ROOT failed (the fake model moved the root back to `database_growth`). All other
  gates passed: 73/74 Safe Pass, top-1 46/47, 0/16 decoy false roots, 27/27 abstentions, 0
  forbidden actions; the LLM arm 74/74. The same scenario passed in the runway shard, in 3
  repeats of the disk family alone, and in a run with the four reactive families before it
  (order effect ruled out): 1 failure in 7 noise runs. Cause (reading `causal/diskwal.go`,
  `wal_score.go`): under the noise load the WAL rate exceeds 10x its long-run average
  (`write_surge` 0.5 + 0.2), while `database_growth` gets its +0.2 only when the databases
  account for at least half of the disk-usage growth; when the load's WAL inflates `pg_wal`
  the share bonus is lost (0.5) and the surge wins. This is in the runway matcher, unchanged
  by the merge; see "Unresolved".
- Shards (PG17): core, reactive and runways each passed every live-arm gate.

### Coverage Gaps (packages below threshold)
None. Lowest business packages: `cmd/pg_sage_sidecar` 71.7%, `internal/store` 74.5%.
`sre-bench` is 60.1% when the gated bench is skipped and 81.2% with it.

### Bugs Found This Session
1. [BUG] `probes/catalog_m6.go`, `catalog_runway.go`: five M6 probes did not declare
   `pg_read_all_stats` (CHECK-08 regression after merge). Fixed.
2. [BUG] causal graph: two branches declared `var v3Nodes` (build break). Renamed.
3. [BUG] runway plans skipped M5's signal probes after the merge would have taken either
   side's plan builder. Unified in `c.plan`.
4. [BUG] test helper collisions (`restrictedPool`, `operatorCtx`) broke `go vet`. Fixed.
5. [TEST BUG] `TestSRESignals_PagePolicy` default assumption. Fixed (base too).
6. [DOC BUG] `forecaster.disk_capacity_bytes` "0 = auto-detect". Fixed.
7. [FINDING, open] disk/WAL runway noise scenario can name `write_surge` under heavy load.

### Manual Checks Remaining
- CHECK-29 / M5 UI: MANUAL. The merged web (SLOs page, Runbooks page, Cases panel with
  proposals, runbook run and similar incidents) passed component tests and a production
  build, not a browser session.
- CI: MANUAL. The split workflow is contract-tested locally; its runtime on GitHub runners is
  unverified until it runs.

## Post-test audit

- **Inputs not covered:** the event-type test scans string literals; an event type built at
  run time (none today) would escape it. The CI contract cannot know GitHub runner speed.
  The bench under `-race` was not rerun after the merge (each branch ran it); unit tests of
  `sre-bench` ran under `-race`.
- **Assertions that pass when broken:** mutation-tested the three new tests (event types,
  plans with signals, CI split): each fails on the injected fault.
- **Fakes:** the plan test builds a bare `Coordinator`; the live coordinator tests on real
  PostgreSQL cover the same `c.plan` path.

## Unresolved / for the coordinator

1. **Runway noise flake** (`disk-database-growth-under-load`, about 1 in 7 under load): the
   runways owner should either let `write_surge` be a contributing factor (amplifier) of
   `database_growth` when both are supported, or compute the growth share against disk
   growth excluding `pg_wal`. Until then the all-families bench can fail CHECK-42-NOISE on a
   loaded runner; the CI shard ran clean here.
2. Load-sensitive tests on the shared machine: the lease tests (PR #61) and
   `TestComposedSRE_M6_DetectorOpensAnLWLockInvestigation` (90 s budget).
3. Reactive decision 4 (detector episodes do not create `sage.incidents`) and runbooks
   decision 4 (no MCP sign tool) stand as the branches decided.
