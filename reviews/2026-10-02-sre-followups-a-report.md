# Sage SRE follow-ups, part A: detector thresholds, detector incidents, report retention

Branch `claude/sre-followups-a`, from master `fce3674` (v1.8.0-to-be). Scope from
`~/.claude/tasks/todo-2026-10-02-lifeos-and-followups.md` item E (part A). Context: the M6
reactive report (decision 4, "what is left"), the M6 integration report (coordinator item 3)
and the M7 report ("`sre_eval_runs` ... retention is a follow-up").

## What was built

| Area | Files | What |
|---|---|---|
| Detector thresholds | `internal/config/sre_detectors.go` (new), one field each in `config/sre.go` and `config/sre_autonomy.go`; `cmd/pg_sage_sidecar/database_runtime_sre_m6.go` | `sre.detectors.window_seconds` (300), `checkpoint_requested` (3), `temp_file_mb` (1024), `lwlock_waiters` (8), `lwlock_polls` (3), `cooldown_minutes` (30): today's values as defaults, validated ranges, doc tags, YAML-only (registry in `store/config_consistency_test.go`), restart lifecycle. `sreDetectorConfig` maps them into `sre.DetectorConfig`; the sidecar warns at startup when the window cannot hold two trigger polls. |
| Detector incidents (sre) | `internal/sre/detect_incident.go` (new), `internal/sre/detect.go`, `internal/sre/triggers.go` | `sre.EpisodeSink` / `Episode` / `EpisodeIncident`. Each poll of an open episode is handed to the sink with the measured evidence and its threshold. Once the sink records the incident, every trigger of the episode is the incident trigger (case id from the Cases projection, incident id, subject, `incident:<id>` key), built by the same helper the RCA trigger source uses (`incidentTrigger`). The detector signals `sre_checkpoint_storm`, `sre_temp_file_explosion` and `sre_lwlock_contention` map back to their families, so the RCA trigger source resumes them after a restart under the same key. |
| Detector incidents (rca) | `internal/rca/episode.go` (new) | `Engine.ObserveEpisode`: the first observation of an episode opens a `warning` incident (or counts an occurrence on its open incident, or links an open RCA incident of the same family with at least its severity). Later observations refresh it and keep it firing until the next analyzer cycle, the way the lock-chain fast path does. An incident resolved during its episode is never reopened by that episode. Identity, dedup, supersede, escalation, auto-resolution, hydration and persistence are the engine's existing ones. |
| Wiring | `cmd/pg_sage_sidecar/sre_episode_incidents.go` (new); one field and one argument in `database_runtime_sre.go` | `episodeIncidents` adapts the database's RCA engine to the sink. It hydrates, observes, and wakes an incident worker that persists (`PersistIncidents`, so notifications go through the existing incident path) off the trigger poll. Built by `startInvestigator`, the constructor every runtime mode uses; nil with `rca.enabled: false`. |
| Report retention | `internal/retention/cleanup.go`; `.WithControlPool` in `database_runtime_exec.go`; `sre.autonomy.report_retention_days` | New purge rule for `sage.sre_eval_runs` on `ingested_at`, removed from the exemptions. Reports are kept when they are referenced as evidence of a current ledger level or of a pending promotion, or when they are the newest report of a family for their source and database. The rule runs in the control database (the meta database in meta-db mode). |
| Docs | `docs/configuration.md`, `docs/generated/config-lifecycles.md`, `web/src/generated/config_meta.json`, `internal/api/dist` (rebuilt, separate commit), `CHANGELOG.md` | The detector paragraph now describes the incidents and the new tables. |

No schema change: `sage.incidents` already accepts `source = 'deterministic'`. No migration was
added and `bootstrap.go` is untouched.

## Product decisions (and why)

1. **Episodes go through the RCA engine; they are never written to `sage.incidents` directly.**
   The engine is the single owner of incident state (identity, dedup, escalation,
   auto-resolution, durable operator resolutions and notifications). A second writer would
   have duplicated that logic and raced with hydration. As a result, with `rca.enabled: false`
   there is no incident, and the investigation keeps its `sre:detector:<family>:<db>` case, as
   before.
2. **One investigation per incident, not per episode.** Detector triggers now use the incident
   key `incident:<id>`, like every other RCA trigger, so the RCA trigger source and the
   detector coalesce (CHECK-13). This changes M6 decision 1 ("one investigation per episode"):
   - A new episode while its incident is still open (inside `rca.dedup_window_minutes` of the
     last observation) counts one more occurrence and does not start a second investigation.
   - Once the incident has resolved or been superseded, the next episode gets a new incident
     (linked as a recurrence) and a new investigation.

   This is what an operator wants: a recurring condition stays one case. It also cuts duplicate
   LLM spend.
3. **Severity `warning`, escalated by the engine.** The detector's thresholds are conservative,
   so an episode starts as a warning. After `rca.escalation_cycles` occurrences of the same open
   incident (default 5 episodes), the engine's existing rule escalates it to critical and sends
   `incident_escalated`. I did not add any detector-specific critical thresholds.
4. **Attach to an open RCA incident of the same family only at equal or higher severity.**
   - The log-based "checkpoints too frequent" incident is a warning, so a checkpoint storm uses
     it, and that incident's case, notification and investigation stand. The log incident is
     linked and never rewritten.
   - The log-based temp-file incident is `info`, one per spill. A temp-file explosion therefore
     opens its own warning incident, so the page is not lost behind an info row.
   - LWLock contention has no RCA signal.
5. **An incident an operator resolves is not reopened by the same episode.** The episode keeps
   its link, and only the next episode (after the cooldown) can open a new incident. This is
   the per-episode dedup the brief asks for, and it respects R04 (durable resolutions).
6. **Detection never waits for the incident store.**
   - If the sink records nothing (store unreachable, or the 500 open-incident cap), the episode
     falls back to the legacy detector trigger. The failure is logged once per family and
     error, and the next poll retries.
   - If the engine tracked the incident but its row could not be written, the trigger is still
     linked: the id is stable, and the row is written on the next pass.
   - Residual: a transient store failure on an episode's first poll can produce one legacy
     investigation plus one incident-linked investigation for that episode.
7. **Persistence and notification run off the trigger poll.** `incident_detected` may wait up
   to 45 s for LLM narration. The incident worker does that, so the investigation starts on the
   same poll.
8. **A window shorter than two trigger polls is a startup warning, not a load error.**
   Refusing it would break an existing `sre.trigger_interval_seconds` above 150 after the
   upgrade (the existing test with 600 must keep loading). The warning says exactly which two
   keys conflict.
9. **The thresholds are YAML-only and restart-bound, with no new enable switch.**
   `sre.automatic_start` already turns detection off, and lowering a threshold makes the
   sidecar open more incidents, so it should not be an API override.
10. **Report retention:**
    - It uses `ingested_at`, default 90 days, range 30-3650. The floor is the spec's 30-day
      bench and game-day evidence window, so retention cannot decay earned autonomy.
    - `generated_at` was rejected. `bench_results_path` is re-read every hour and reports are
      deduplicated by hash, so an old file still in the directory would be re-ingested and
      deleted on every pass. With `ingested_at` that cycle is once per retention period.
    - Kept: any report whose id appears in the evidence of a current `sre_family_autonomy` row
      or a pending proposal (jsonpath `$.**.id`, so this does not depend on the evidence's
      shape), and the newest report of each family per source, and per database for game days
      (ordered by `generated_at`, `ingested_at`, `id`, as `LatestBench` reads).
    - This overrides M7 decision D13 ("M7 tables exempt") for `sre_eval_runs` only. The other
      M7 tables stay exempt.
    - A game-day row's `eval_run_id` can point at a pruned report. Nothing reads reports by id.
11. **Rebuilt dist committed separately** (`3065a2b`), so it can be regenerated at integration
    if another branch also rebuilds it.

## Spec CHECKs covered

- **CHECK-06** (normal activity does not become an urgent incident without evidence):
  thresholds are unchanged, severity is warning, and the evidence carries the measurement and
  threshold (`TestDetectorIncident_EvidencePerFamily`, `TestObserveEpisode_PersistsAndNotifiesOnce`).
- **CHECK-07** (resets and restarts never read as change): the detector's reset handling is
  unchanged (`TestDetector_CounterResetIsNotGrowth`). After a restart the engine hydrates the
  open detector incident and a new episode attaches to it
  (`TestObserveEpisode_AttachesToAHydratedIncident`).
- **CHECK-13** (duplicate and out-of-order triggers coalesce): the RCA trigger and the detector
  trigger for one incident are identical, and 16 racing starts give exactly one investigation
  (`TestDetectorIncident_TriggerEqualsTheRCATriggerAndCoalesces`). Concurrent first
  observations are one incident row (`TestEpisodeIncidents_ConcurrentFirstObservationsAreOneIncident`).
- **CHECK-27** (absent, zero or conflicting config keeps safe defaults):
  `TestSREDetectorsDefaults_NoConfigFile`, `_PartialSectionKeepsTheRest`, `_Boundaries`
  (explicit zeros refused), `_SlowTriggerIntervalStillLoads`,
  `TestSREDetectorConfig_DefaultsMatchTheDetector` (an unloaded section keeps the defaults).
- **CHECK-28** (migration idempotent, upgrade keeps incidents and actions): no schema change.
  Existing incidents are untouched; legacy detector investigations keep their cases.
- **CHECK-29** (the Cases panel opens the evidence): the investigation's case id equals the
  Cases projection of the incident row, and the case lists exactly that investigation
  (`TestComposedSRE_DetectorEpisodeOpensAnIncidentCaseAndInvestigation`).
- **CHECK-31** (every runtime mode wires the same constructor): the sink is built in
  `startInvestigator` and the cleaner in `startExecution`, both shared by every mode.
- **CHECK-35** (existing RCA, collector, action and retention behavior still works): the
  RCA, retention and composed suites pass.
- **§13 M7**: a detector-linked investigation has its family and counts as one more accepted
  shadow review (`reviewDetectorInvestigation` in the composed test). Report retention never
  removes ledger or pending evidence (`TestRun_AgesOutEvalRunsButKeepsEvidenceAndNewestPerFamily`).

## Test Results

**Command (full suite, PG17 `pgsage-ag5`, repo root mounted):**
`go test -count=1 -cover -v ./...` in `golang:1.25`, with `SAGE_TEST_DATABASE_URL` set to
`:55475`.

**Total:** 7737 passed, 2 failed, 14 skipped. Both failures are load timeouts in packages
this branch does not touch, and both pass when rerun alone (`ok` for both):

- `internal/sre/probes` `TestCatalog_ReplicationProbesOnAPrimaryWithoutReplicas`: the probe
  hit its 500 ms `statement_timeout` while every package ran in parallel.
- `sre-bench` `TestReplayCorpus`: "context deadline exceeded" at 180 s. The package takes
  about 200 s alone, so it is close to its budget.

The touched packages were run again after the last fix (`a32d38c`): `-race` on PG17,
`cmd/pg_sage_sidecar` on PG18, and the new episode tests.

**Coverage of touched packages (PG17):**

| Package | Coverage |
|---|---|
| internal/config | 89.2% |
| internal/rca | 96.0% |
| internal/retention | 100.0% |
| internal/sre | 87.3% (87.4% on PG14/PG18) |
| internal/store | 74.5% |
| cmd/pg_sage_sidecar | 72.7% |

All packages meet the coverage thresholds: every touched package is at least 70%.

**Cross-version (touched packages):**

- **PG14 (:55414) and PG18 (:55418):** config, rca, retention and store are `ok` on both.
  `internal/sre` and `cmd/pg_sage_sidecar` are `ok` on both.
  - The first run started both versions in parallel next to other agents' load. It hit
    bootstrap advisory-lock and 10-minute package timeouts in tests this branch does not
    touch (for example `TestMetaLifecycleCreateReplaceDelete` and
    `TestDurability_OutageBlocksHandoffUntilVerified`).
  - Sequential reruns with `-timeout 40m` pass. Two load failures remained in those reruns:
    `TestModelProbe_InconclusiveGraphConcludesAfterTheProbe` (bootstrap lock) on PG14, and
    `TestEpisodeIncidents_ConcurrentFirstObservationsAreOneIncident` on PG18, whose 60 s
    fixture expired. The PG18 one revealed bug 4 below. Both pass on rerun: PG14 `sre` `ok`
    after the fix, and PG18 `cmd` `ok` with `a32d38c`; the episode tests ran 3 times on PG18
    and passed each time.
- **`-race` (PG17)** on config, rca, retention, sre, store and cmd/pg_sage_sidecar: all `ok`,
  with no data race.
- **`golangci-lint run ./...`:** 0 issues.
- **Web (`sidecar/web`):** `npm test` passes 246 tests in 46 files, and `npm run build`
  succeeds; the dist is committed.

### Skipped Tests (must be zero or justified)
- agentdb: 6 live provisioning and gauntlet tests. They need `PG_SAGE_LIVE_AWS_RDS`,
  `_GCP_CLOUDSQL`, `_DATABRICKS_LAKEBASE` or `_AGENTDB_GAUNTLET`.
- azure: 2 live parameter tests. They need `PG_SAGE_LIVE_AZURE`.
- llm, rca: `TestChatWithToolsLive_RealProvider` and `TestTier2Live_RealGemini` are live LLM
  tests (`PG_SAGE_LIVE_LLM`, no credits; item H).
- sre-bench: `TestPGIncidentBench` needs `SAGE_BENCH_RUN=1`; CI runs it in its own step.
- rca: `TestRCAChildProcessFixture` is a helper that runs only as a child process.
- logwatch: `TestResolveLogDir_AbsoluteWindows` covers Windows paths and is skipped in the
  Linux container.
- collector: `TestCycle_RotatesSnapshots` is a timing-guarded skip that predates this branch.

None of the skips are in tests added here.

### Failures
None in the touched packages. The full-suite failures above are load timeouts in untouched
packages and pass on rerun.

### Coverage Gaps
All touched packages meet their thresholds (business ≥ 70%). The lowest are
`cmd/pg_sage_sidecar` at 72.7% and `internal/store` at 74.5%. Both are unchanged by this
branch apart from new code, and that new code is covered.

### Manual Checks Remaining
- CHECK-FA-UI: MANUAL. A detector incident's case in the Cases panel, in a browser (component
  and API identity are tested; the look is not).

## Bugs found

1. [BUG, fixed by this work] Similar-incident memory (`postgres_memory.go`) excluded every
   earlier detector investigation of the same family, because all of them shared one case id
   (`sre:detector:<family>:<db>`) and one trigger fingerprint (constant subject), and the
   leakage guard drops candidates with the target's case or fingerprint. Detector
   investigations now have per-incident case ids and subjects, so earlier episodes are eligible
   as similar incidents.
2. [BUG, pre-existing, fixed] `internal/retention/cleanup.go` on master was not gofmt-clean (two
   misaligned exemption entries). gofmt was applied, since the file is touched here.
3. [TEST GAP, found by mutation] E2: a later poll could relink an episode to another incident
   with no test failing. R5: an observation that only reset the clear count, without counting
   as firing, survived; it is visible with `rca.resolution_cycles: 1`. Both are pinned in
   `d00ee7a`.
4. [BUG, fixed in `a32d38c`] A PG18 run under load showed one failed persistence pass of the
   episode worker leaving the incident row (and its notification) until the next episode
   observation or analyzer cycle (up to ~10 minutes). The worker now retries a failed pass every
   5 s (`TestEpisodeIncidents_FailedPersistRetries`, committed first in `36433f9`; mutant W4
   "no retry" is killed by it).

## Mutation testing

27 mutants, all killed (script in the session scratchpad). E4 was first killed only by a
compile error; it was rewritten as a semantic mutant and is killed by a test.

| Id | Mutant | Killed by |
|---|---|---|
| E1 | A new episode keeps the previous episode's incident | `TestDetectorIncident_NewEpisodeAfterCooldownAsksAgain` |
| E2 | A later poll overwrites the episode's incident | `TestDetectorIncident_EpisodeIsNeverRelinked` (added) |
| E3 | Severity critical | `..._EpisodeOpensOneIncidentAndLinksEveryTrigger`, `..._EvidencePerFamily` |
| E4 | No related RCA signals | `TestDetectorIncident_EvidencePerFamily` |
| E5 | A sink error is not logged | `..._SinkFailureFallsBackAndRetries`, `..._PersistFailureStillLinks` |
| E6 | Checkpoint evidence drops the timed count | `TestDetectorIncident_EvidencePerFamily` |
| R1 | Never attach to a related incident | `TestObserveEpisode_AttachesToARelatedIncidentOfAtLeastItsSeverity` |
| R2 | Attach ignores severity | `TestObserveEpisode_DoesNotAttachBelowItsSeverity` |
| R3 | Attach needs strictly higher severity | `..._AttachesToARelatedIncidentOfAtLeastItsSeverity` |
| R4 | A resolved incident is refreshed | `..._ResolvedIncidentIsNotReopenedByItsEpisode`, `..._PersistsAndNotifiesOnce` |
| R5 | A refresh does not count as firing | `TestObserveEpisode_ObservationCountsAsFiringForTheNextCycle` (added) |
| R6 | A new episode is not counted as an occurrence | `..._NewEpisodeAttachesToTheOpenIncident`, `..._AttachesToAHydratedIncident` |
| R7 | An invalid severity is accepted | `TestObserveEpisode_InvalidEpisodes` |
| T1-T2 | No ledger keep / no pending-promotion keep | `TestRun_AgesOutEvalRunsButKeepsEvidenceAndNewestPerFamily` |
| T3-T5 | "Newest" ignores the database, compares NULL with `=`, or ignores the deployment | the same test, plus the boundary, control-pool and concurrency tests (T4) |
| T6 | The control pool is ignored | `TestRun_EvalRunRetentionUsesTheControlPool` |
| T7 | `generated_at` as the time column | `TestRetentionRules_EvalRunsAreAgedOut` |
| C1-C3 | Window floor 30, detectors not validated, retention floor 1 | config boundary tests |
| W1 | Temp MiB mapped as KiB | `TestSREDetectorConfig_MapsEveryKnob` and 3 more |
| W2 | Window warning off by one | `TestSRETriggerSource_WarnsWhenTheWindowCannotSeeGrowth` |
| W3 | The sink never wakes persistence | `TestEpisodeIncidents_*` (rows never written) |
| W4 | A failed persistence pass is not retried | `TestEpisodeIncidents_FailedPersistRetries` (added) |

## Post-test audit

1. **Inputs not tested.**
   - A sink that returns an incident of another database. This cannot happen with the engine,
     which stamps its own database name.
   - A hung monitored database during `Hydrate` on the first episode. This blocks that trigger
     poll exactly like the existing RCA trigger read on the same pool (no new exposure). There
     is no per-poll deadline in the coordinator loop (pre-existing).
   - Several sidecars monitoring the same database each keep their own engine and could each
     open an incident (pre-existing for all RCA incidents).
2. **Assertions that would pass when broken.** Mutation testing found two (E2, R5); both are
   fixed. The composed test asserts the incident row, the case equality, exactly one
   investigation in the case, one `incident_detected` and the shadow count delta, not only
   errors.
3. **Fakes that hide failures.**
   - `fakeSink` in the sre unit tests. The real engine and store are covered by
     `TestEpisodeIncidents_*`, `TestObserveEpisode_*` (DB) and the composed test.
   - The composed test adds temp-file growth on top of the real `temp_file_activity` row (a
     real 1 GiB spill in CI is slow and flaky). The rest of the investigation runs real probes.
   - LLM narration of the notification is the existing path; it is off in these tests (LLM
     disabled) and exercised by the RCA narration tests.
   - The retention rule runs real SQL on PG14, 17 and 18, including the jsonpath (PG12+).
4. **Shared files.** One field, one default and one range line in `config/sre.go` and
   `config/sre_autonomy.go`, one deps field and one argument in `database_runtime_sre.go`,
   one chained call in `database_runtime_exec.go`, one rule and one predicate in `cleanup.go`,
   three map entries in `triggers.go`, and one CHANGELOG bullet. All new files are ≤ 500
   lines, and all new functions are ≤ 50 lines.

## What is left

- Live LLM narration of a detector incident's notification (blocked on credits, item H).
- A detector incident's root cause is the family line and its measurement. Whether the
  investigation's conclusion should be written back onto the incident (as RCA Tier 2 does) is
  a separate decision for every investigation family, not only detector ones.

## Coordinator decisions

1. **Conflicts to expect.**
   - `config/sre_autonomy.go`: `claude/fast-trust` edits the same struct, default and range
     list. My edits are one line in each.
   - `retention/cleanup.go`: `claude/debt-exec` ("retention delete via Apply") may edit it. Mine
     is one rule, one constant predicate, `WithControlPool` and the control-table dispatch in
     `Run`.
   - `internal/api/dist`: drop `3065a2b` and rebuild once after merging if other branches
     rebuilt too.
2. **If fast-trust makes the bench evidence age (`BenchMaxAge`) configurable above 30 days,**
   the `report_retention_days` floor should follow it. Otherwise game-day evidence still inside
   a longer window could be pruned. Today the floor equals the spec constant.
3. **Confirm "one investigation per incident" (decision 2).** It replaces M6's "one
   investigation per episode" while an episode's incident stays open.
