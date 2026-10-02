# Sage SRE M5 part 2: SLOs, burn rates, change events (report)

Branch `claude/sre-m5-slo` (from the M3 branch `claude/sage-sre-m3-llm`). Spec:
`reviews/2026-09-26/AI-SRE-SPEC.md` §4 R1 change feed v0 and R1.1, §7.3, §8, §9, §10, §12.
Approved actions, ChatOps and `request_execution` (M5 part 1) and the autonomy levels (M7)
are not in this branch.

## What was built

| Area | Files | What it does |
|---|---|---|
| Signed ingestion | `internal/sre/signed/signed.go` | HMAC-SHA256 (`v1=`) over `v1:ts:METHOD:path:body`, timestamp tolerance (default 300 s), distinguishable errors (`ErrNotConfigured`, `ErrMissing`, `ErrMalformed`, `ErrStale`, `ErrInvalid`). Stateless. |
| Change feed v0 | `internal/sre/changefeed/{event,store,feed,poller,sources}.go` | One durable feed (`sage.sre_change_events`). External kinds (deploy, migration, feature_flag, config, other) arrive signed; internal kinds come only from pg_sage's own sources: `sage.action_log`, `sage.config_audit` (key and actor, never values), migration-detector DDL (`sage.findings` `migration_safety`), `pg_stat_statements_info.stats_reset`, `pg_postmaster_start_time()` (restart), `pg_is_in_recovery()` and `pg_control_checkpoint().timeline_id` (failover), `pg_extension` versions. Snapshot sources record a baseline first and emit on change; cursor sources backfill 7 days. A source that cannot be read is reported (`no_privilege`, `unsupported`, `not_configured`, `error`), never healthy. 90-day retention. `change_feed` signal probe. |
| SLO layer | `internal/sre/slo/*.go` | Canonical SLI bad/eligible. Sources: Prometheus-compatible connector (`query`, `query_range`, bearer token, timeout, typed errors, token never in errors), pushed cumulative counters per series (idempotent per series and time, resets counted), and 4 database proxies. Multi-window multi-burn-rate evaluation (workbook defaults, configurable). Unknown for no data, stale (> 5 min), partial window (< 90% covered), zero denominator, low traffic (< `min_eligible_events`), invalid values, an unconfigured connector, an evaluation older than three intervals. Durable state (`sage.sre_service_slos`, `sage.sre_slo_transitions`), burn start kept across a page→ticket step. `ErrorBudgetSource` (`BudgetSummary`) and `RecoveryPredicate` interfaces. `slo_status` signal probe. Prometheus `pg_sage_slo_*` metrics. |
| Database proxies | `slo/proxy.go`, `proxy_latency.go`, `proxy_errors.go` | `db_latency`: per query_store capture interval, calls-weighted p95 of the top-20 queries' interval mean latency; bad above 3x the database's own 7-day median p95 (floor 50 ms; absolute threshold optional; `baseline_building` until 30 values; a statistics reset is not a slice). `db_errors`: server-class SQLSTATE log errors (53, 57, 58, XX, 40, 08, 55P03) per transaction; needs a log watcher, else `source_unavailable`. `db_connection_refusal`: minutes with every client slot in use. `db_replication_lag`: minutes with replay lag over 60 s (`no_replicas` on a primary without standbys). |
| Investigator | `internal/sre/{signals,plan,coordinator,worker,record,conclusion,request}.go` | `CoordinatorDeps.Signals`: wired signal probes join the first step of every plan. New trigger `slo_burn` (triage plan: lock graph, prepared xacts, long transactions, connection saturation, plan regressions, pg_sage actions, + signals: 8 probes). `Summary.CustomerImpact` bound to the `slo_status` evidence id (scope-checked like every fact). Without signals the M2/M3 plans are unchanged. |
| Causal graph v3 | `internal/sre/causal/{signals,triage,graph,match}.go` | Node `recent_change` (refutation probe `change_feed`): supported by the feed's changes in the window (pg_sage's own actions stay `sage_own_action`), ruled out by an observed empty feed, missing when the feed is unreadable, never a root on its own (0.1 per change, at most 3). `WithSLO`: observed facts per SLO (proxies labeled "a proxy, not customer impact"), customer impact only from app SLIs. `DiagnoseSLOBurn`: the most confident conclusive lock/connection/plan root wins, others stay unproven; with none, inconclusive "the cause may be outside PostgreSQL". `GraphVersion` is now `causal-v3`. |
| Schema | `internal/schema/sre_m5_slo_migration.go` (+1 line in `bootstrap.go`) | `sre_service_slos`, `sre_slo_transitions`, `sre_sli_samples` (CHECK bad <= eligible), `sre_change_events` (unique deployment/source/event id), `sre_change_feed_state`. Idempotent; retention exemptions recorded in `retention/cleanup.go` (owned by the engine and the poller). |
| Config | `internal/config/sre_slo.go`, `sre_slo_validate.go` (+fields in `sre.go`, `clone.go`) | `sre.slo.*` and `sre.change_events.*`, validated with key-naming errors that never echo secrets; deep-copied by `Clone`. Generated `docs/generated/config-lifecycles.md` and `web/src/generated/config_meta.json` regenerated (secrets flagged). |
| API | `internal/api/sre_signal_handlers.go`, `sre_slo_handlers.go` (+1 line router, +3 lines auth skip) | `POST /api/v1/sre/change-events` and `POST /api/v1/sre/sli/{name}` (no session: signature only; 503 `not_configured` without a secret; 401 `missing_signature`/`invalid_signature`/`stale_timestamp`; 403 `source_not_allowed`; 409 `conflict` for a reused id with other content; replay → 200 `duplicate: true`). Viewer reads `GET /api/v1/sre/slos[?database=]`, `GET /api/v1/sre/slos/{name}[?database=&since=]` (status, transitions, recovery), `GET /api/v1/sre/changes?database=&window_minutes=`. |
| MCP | `internal/mcp/signal_tools.go`, `production_signals.go` (+2 small edits) | `sre_list_slos`, `sre_get_slo` (with `recovery_since`), `sre_list_changes`: read-only, strict typed arguments. |
| Wiring | `cmd/pg_sage_sidecar/{sre_signals,sre_signals_config,database_runtime_signals,mcp_signal_access,slo_metrics}.go` | Same constructor in every mode (CHECK-31): feed, poller and engine bind lazily to the investigator's scope; the error proxy subscribes to the cluster log fanout when one runs. |
| Web | `web/src/pages/SLOsPage.jsx`, route `#/slos`, nav item "SLOs"; dist rebuilt | State, burn rates per rule, budget left, unknown reasons, customer impact, and with one database selected its change feed (24 h). |

## Product decisions (and why)

1. **Proxies and the change feed are on by default; app SLIs need configuration.** They are
   read-only, need no external setup and are always labeled proxies (AI-DBA principle). The
   connector and the ingestion endpoints are off until a URL or a 32-byte secret is set.
2. **Self-calibrating latency proxy.** A fixed threshold would page analytics databases
   forever; the default compares with the database's own week (3x median, 50 ms floor).
   It says `baseline_building` until it has 30 values instead of guessing.
3. **Who opens investigations.** A page-level burn of a registered app SLI opens a
   read-only `slo_burn` investigation by default (`sre.slo.open_investigations`), because the
   operator registered that SLI. A proxy burn opens one only with `sre.automatic_start`
   (R1 keeps automatic starts opt-in). One investigation per burn (idempotency key = SLO +
   burn start); a continuing burn coalesces.
4. **Unknown is never ok, but a seen burn wins.** A rule with an unknown window is unknown;
   any firing rule pages/tickets even if another rule is unknown. The factor is inclusive.
5. **Error-budget source for M7.** `BudgetSummary` reports `fast_burning` (any SLO, app or
   proxy, firing a page rule), `app_fast_burning`, and the names of unknown SLOs. It is read
   from the durable state (survives restarts) and goes unknown after three missed
   evaluations. Recommendation for M7: downgrade to <= L1 on `fast_burning`; treat unknown
   as "cannot certify" (no promotion), not as burning, or every install without an app SLI
   would be pinned at L1.
6. **Recovery predicate.** Customer recovery is certified only by the newest three 2-minute
   slices, each with fresh data, no counter reset, at least `min_eligible_events`, at least
   half the pre-incident traffic, and a burn under 1x. A clearly burning slice is
   `not_recovered` even next to unknown ones (CHECK-22, CHECK-32).
7. **Prometheus semantics.** An empty bad query is `no_data` (unknown), so the config doc
   asks for `or vector(0)`. Counter resets are compensated by `increase()`; an optional
   `resets_query` makes them visible to the recovery predicate.
8. **Replay protection.** Signatures expire with the tolerance; inside it, a replay hits the
   unique (source, event id) or (SLO, series, time) key and is a no-op (200 `duplicate`).
   The same id with different content is a 409, not a silent overwrite. Internal kinds
   (restart, failover, sage_action, ...) cannot be submitted, so they cannot be forged.
9. **Where state lives.** All new tables are in the coordination database (meta DB, else the
   monitored/primary control DB), keyed by the deployment and the stable database UUID like
   the other `sre_*` tables. Pushed samples are deployment-wide (an SLO named for no
   database applies to every database and is evaluated per database); purges are per SLO.
10. **Graph version bump** to `causal-v3`: an investigation pins the graph it used, and v3
    adds a node. Other M5/M6 branches may also bump; the merge keeps the highest.
11. **UI.** The dashboard had a cheap place for it, so there is an SLO page. `internal/api/dist`
    is tracked, so it was rebuilt in its own commit (`build(web)`); after merging other UI
    branches the coordinator should rebuild dist once.
12. **Config lifecycle.** All new keys are `restart` (the generator default); nothing here
    hot-reloads.

## Spec checks covered

- **CHECK-22** (verification rejects stale telemetry and false success after traffic
  disappears) for the SLI recovery predicate: `slo/recovery_test.go`,
  `TestEngine_Recovery`, `TestEngine_RecoveryPrometheus`.
- **CHECK-23** (concurrent deployment attributed, not hidden): signed deploys and every
  change-feed source appear as cited `recent_change` evidence in every investigation:
  `TestCoordinator_SignalsJoinEveryInvestigation`, `TestSRESignals_BurnToInvestigationWithEvidence`.
- **CHECK-32** (app SLI no-data / counter reset / low traffic cannot certify recovery):
  `TestRecovery_CannotCertifyWithoutTrustworthyData`, `TestEngine_Recovery`.
- **CHECK-33** (operates with external telemetry unavailable): a missing connector, an
  unreachable Prometheus, an unbound feed or SLO store are unknown/missing evidence and the
  investigation still concludes on database evidence (`TestEngine_PrometheusObjective`,
  `TestFeed_ProbeEmptyAndUnavailable`, `TestWithChanges_UnavailableAndNotCollected`).
- **CHECK-31** (same constructor in every mode): `startInvestigator` builds the signals for
  standalone, YAML fleet and meta fleet alike; existing runtime parity tests pass.
- **CHECK-37/38** extended: `recent_change` has a refutation probe; pg_sage's own actions
  are both `sage_own_action` and change-feed events.
- **CHECK-40** (autonomy downgrade on burn): the input side only. `ErrorBudgetSource` and
  `fast_burning` are implemented and tested here; the downgrade itself belongs to M7.
- **CHECK-34** (live provider tests prove exact permissions): partially. The connector is
  tested against an in-process Prometheus-compatible server (auth header, query/query_range,
  every error class, timeout); there is no live Prometheus in this environment. The change
  feed's permission behavior is proven live with a role that cannot read `sage` (sources
  reported `no_privilege`, the rest keep working).

## Test Results

**Command (touched packages, PG17 `pgsage-ag2`):**
`go test -json -count=1 -cover ./internal/sre/... ./internal/api/ ./internal/mcp/ ./internal/config/ ./internal/schema/ ./internal/retention/ ./internal/fleet/ ./internal/store/ ./cmd/pg_sage_sidecar/`
(in Docker `golang:1.25`, worktree root mounted, `SAGE_TEST_DATABASE_URL` set).
**Total:** 2710 passed (tests and subtests), 2 failed, 0 skipped.
**Full suite (`go test -count=1 -cover ./...`, PG17):** every package passed except
`internal/analyzer`, `internal/sre`, `internal/sre/probes` and `internal/store`; the store
failure was a real gap of this branch (fixed, see Bugs), the others are load timing (below)
and pass when rerun.
**Matrix:** touched packages on PG14 (`:55414`) and PG18 (`:55418`): all pass except the same
load-timing `internal/sre`/`internal/sre/probes` lease tests on PG14 (PG18: all pass).
**Race:** `-race` on `signed`, `changefeed`, `slo`, `causal`, `probes`, `mcp`, `config`, `api`,
`cmd/pg_sage_sidecar`: no data race reported; all pass after the `TestPoller_RunStopsWithContext`
fix. `internal/sre -race` fails only the lease-timing tests below.
**Lint:** `golangci-lint run ./...` 0 issues. **Web:** `npm test` 40 files / 200 tests pass,
`eslint` clean on touched files, `npm run build` done (dist committed separately).

**Coverage (touched packages):**

| Package | Coverage | Threshold |
|---|---|---|
| internal/sre/signed | 100.0% | 70% |
| internal/sre/slo | 89.4% | 70% |
| internal/sre/changefeed | 89.4% | 70% |
| internal/sre/causal | 95.8% | 70% |
| internal/sre/probes | 90.9% | 70% |
| internal/sre | 88.2% | 70% |
| internal/api | 75.2% | 70% |
| internal/mcp | 80.6% | 70% |
| internal/config | 88.1% | 70% |
| internal/schema | 81.6% | 70% |
| internal/retention | 100.0% | 70% |
| internal/fleet | 82.9% | 70% |
| internal/store | 74.2% | 70% |
| cmd/pg_sage_sidecar | 70.5% | 70% |

All packages meet the coverage thresholds.

### Skipped Tests
- None in the touched packages (live PostgreSQL was configured for every run).
- `TestPoller_StatsReset` passes either way: with `pg_stat_statements` unavailable in the
  fixture database it checks the `unavailable` path and logs why (not a skip).

### Failures (if any)
- `internal/sre`: `TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep`,
  `TestCoordinatorRun_WithoutAutomaticStartOnlyResumes` (and, in other runs,
  `TestStore_StaleWorkerCannotCommit`, `TestStore_PendingFindsQueuedAndOrphanedWork`,
  `TestStore_ActiveTimeIsChargedAndCapped`, `TestModelTurn_SlowCallKeepsTheLease`,
  `TestCollect_ResumeAfterNeedsEvidenceConcludes`); `internal/sre/probes`:
  `TestRunner_VersionGate`, `TestRunner_SidecarWideLimit`. All are pre-existing M1-M3 tests
  with 300 ms - 1 s leases or 500 ms statement timeouts. They fail only while 5-9 other
  agents' `go test` and PGIncidentBench containers load the machine. Proof it is not this
  branch: the same subset run at the same moment on the M3 base commit (`e44bd6b`, a
  temporary worktree) failed the same way (7 failures base, 10 branch, `-race -count=3`), and
  on this branch they pass in isolation (`-count=5`). With no signal probes wired (as in
  those tests) the M5 code paths are no-ops.
- `internal/analyzer` `TestPreflightEvidenceStaleSnapshotDoesNotRefreshFinding` failed once in
  the full run (`pg_stat_statements` relation missing in its fixture) and passed on rerun;
  this branch does not touch the analyzer.

### Coverage Gaps (packages below threshold)
- None. Lowest: `cmd/pg_sage_sidecar` 70.5% (package-wide; the new wiring files are
  exercised by `sre_signals_test.go`), `internal/store` 74.2% (only a test list changed).

### Bugs Found This Session
1. [BUG] `slo/engine.go`: a page-level burn was reported again on every evaluation. The burn
   start read back from PostgreSQL (microseconds) never equalled the reported Go time
   (nanoseconds). Found by `TestEngine_FastBurnPagesOnceAndPersists`; evaluation times are now
   truncated to microseconds.
2. [BUG] `slo/store_status.go` (design): purging samples by one engine's longest window would
   have deleted another SLO's (or database's) samples with a longer window. Purge is now per
   SLO; the test checks another SLO's samples survive.
3. [BUG] `changefeed/sources.go`: snapshot comparison of timestamps with `==` compares time
   zones, so every poll would have emitted a false "restart" / "stats reset". Times are
   compared as canonical UTC strings (found while implementing, covered by
   `TestPoller_BaselineThenNoChange`).
4. [BUG] `slo/evaluate.go`, `recovery.go`: `1 - 0.999` is not exactly 0.001, so a burn of
   exactly 14.4x (or exactly 1x in recovery) would not fire. Comparisons use a 1e-9 relative
   tolerance; the boundary tests pin it.
5. [BUG] `config/clone.go`: the new SLO and change-event lists would have been shared between
   a running config and its reload candidate. `Clone` deep-copies them (`TestSRESLOConfig_CloneIsDeep`).
6. [BUG] `internal/store` registry: the new keys were neither runtime overrides nor excluded
   (`TestConfigConsistency_AllowedKeysMatchStruct`, full suite). Marked YAML-only like the
   M2/M3 `sre.*` keys.
7. [TEST] Fixture/timing errors in my own tests, fixed with explanations in the commits: a
   query_store fixture that inserted each row in its own transaction, transaction counts that
   reach `pg_stat_database` asynchronously, a deployment-wide event leaking into other tests'
   windows, a fixed 100 ms wait in the poller `Run` test, and the pre-M1 simulation's table list.

### Manual Checks Remaining
- CHECK-M5-UI: MANUAL. The SLO page in a browser (unit tests cover rendering and the change
  feed; it was not viewed in a running sidecar).
- CHECK-34: MANUAL. A live Prometheus (and a live push client) were not available; the
  connector is proven against an in-process Prometheus-compatible server.

## Post-test audit

- **Mutation testing** (16 mutants, each run against its tests; all killed): both-windows
  rule (`&&`→`||`), stale check, counter-reset and traffic-drop checks in recovery, the
  low-traffic floor, the past-side timestamp tolerance, the conflict-on-changed-content
  check, excluding pg_sage actions from `recent_change`, app-only customer impact, page-once,
  reset compensation in the aggregate SQL, `Clone` deep copy, the source allowlist, the
  stale-evaluation rule, the proxy-burn investigation policy.
- **Inputs not covered by a test, now added**: the server-class SQLSTATE set, the history a
  proxy receives from the engine, an SLO's status before its first evaluation and its
  transitions, Prometheus recovery (with a traffic drop), a submission for another database,
  an installed extension, the unavailability classification by SQLSTATE.
- **Fakes that could hide failures**: the Prometheus connector is tested against an
  in-process HTTP server (real HTTP, real JSON, real timeouts), not a mocked client; every
  store, poller, proxy, engine, API and the full burn-to-investigation chain run against real
  PostgreSQL. The replication-lag proxy's standby branch and lag-over-budget branch are not
  exercised (no standby in the test environment); the error proxy's log counter is a fake in
  `slo` tests (the logwatch adapter is a 10-line drain-and-count wiring in `cmd`).
- **Assertions that pass when broken**: reviewed; every test asserts states, counts, reasons
  or stored rows, not only `err == nil`.

## What is left / for the coordinator

1. **M7 alignment (decide at integration).** `internal/earned` on `claude/sre-m7-autonomy`
   expects `ErrorBudget(ctx, database) (BudgetState{Configured, FastBurning, Unknown,
   Detail})` and downgrades on `Unknown`. Because the proxies are on by default, a proxy is
   often unknown for structural reasons (`no_replicas`, `source_unavailable`,
   `baseline_building`). Mapping `Unknown` to "any SLO unknown" would pin every database at
   L1. Suggested adapter over `inst.SLO.BudgetSummary(ctx)`:
   `Configured = sum.AppSLOs > 0 || sum.FastBurning`, `FastBurning = sum.FastBurning`
   (or `AppFastBurning` if proxy burns should not downgrade), `Unknown = len(sum.UnknownApp) > 0`.
   `AppSLOs` and `UnknownApp` were added for exactly this.
2. **Graph version.** This branch is `causal-v3`; if an M6 branch also bumps, keep one
   version that includes every node.
3. **Shared files touched (small, additive):** `schema/bootstrap.go` (1 line),
   `retention/cleanup.go` (exemptions), `config/sre.go` + `clone.go`, `fleet/types.go`
   (2 fields), `api/router.go` (1 line), `api/auth_middleware.go` (skip signed paths),
   `mcp/server.go` (tool list + dispatch), `mcp/production_backend.go` (1 field),
   `cmd/.../main.go` (1 line), `database_runtime*.go`, `mcp_runtime.go`,
   `store/config_consistency_test.go`, `schema/sre_migration_test.go` (table list), generated
   `docs/generated/config-lifecycles.md` and `web/src/generated/config_meta.json`
   (regenerate after merging), `internal/api/dist` (rebuild once after all UI merges).
4. **Recovery wiring.** `slo.RecoveryPredicate` (by SLO name and intervention time) is ready
   for the M5-actions recovery check after an approved action or a recorded external change;
   an `slo_burn` investigation's subject names its SLO (`slo <name>`).
5. **Not done:** a live Prometheus test (CHECK-34); hot reload of `sre.slo.*` (restart-bound);
   batching of pushed samples; mapping an app SLO to several (not all) databases; the
   connection-refusal proxy uses slot exhaustion rather than counting `53300` log lines; an
   SLO view entry in the Cases panel (the investigation's customer impact is in its summary
   and export, not yet rendered by the Cases UI).
