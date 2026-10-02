# Sage SRE M6 part 1: reactive incident families (report)

Branch `claude/sre-m6-reactive` (based on the M3 branch, `e44bd6b`). Scope: four R2 families
from AI-SRE-SPEC §4, investigated end to end: **checkpoint storm**, **temp-file
explosion**, **replication lag** and **LWLock contention**. Runway families (wraparound,
disk/WAL runway, sequence exhaustion) belong to `claude/sre-m6-runways` and are not touched.

## What was built

| Area | Files | What |
|---|---|---|
| Probes | `internal/sre/probes/catalog_m6.go`, `decode_m6.go`, `extension.go`; edits in `types.go`, `registry.go`, `runner.go`, `classify.go`, `catalog_capacity.go` | 6 new read-only catalog probes and `replication_lag` v2 (see below). Specs may name an extension: the runner resolves its schema inside the probe's read-only transaction and quotes it, so `pg_stat_statements` works in any schema. A missing extension is `unsupported / extension_not_installed`, never "no rows". |
| Causal graph v3 | `internal/sre/causal/graph_m6.go`, `graph_m6_repl.go`, `checkpoint.go`, `temp.go`, `repl.go`, `lwlock.go`, `series.go`; `graph.go` (version `causal-v3`) | 18 mechanisms in 4 families, each with a refutation probe and a manual operator step. Matchers score competing hypotheses, rule out the contradicted ones with the evidence that contradicts them, and treat resets and restarts between samples as unknown deltas (CHECK-07). |
| Investigator | `internal/sre/plan_m6.go`, `detect.go`, `triggers_combine.go`; edits in `request.go`, `triggers.go`, `plan.go`, `worker.go` | Trigger kinds, probe plans (a sampling step can wait several sample intervals), RCA signal mapping, the reactive detector and a combined trigger source. |
| Wiring | `cmd/pg_sage_sidecar/database_runtime_sre_m6.go`, one call in `database_runtime_sre.go` | Every database's investigator polls RCA incidents/findings plus the detector. |
| Bench | `sre-bench/scenarios_ckpt.go`, `scenarios_temp.go`, `scenarios_repl.go`, `scenarios_lwlock.go`, `m6env.go`, `rules_m6.go`, `families.go`; edits in `replication.go`, `rules.go`, `scenarios_lock.go`, `bench_test.go`, `README.md` | 22 fault programs (clean, noise, decoy, benign per family), rules-only baselines, `SAGE_BENCH_FAMILIES`. |
| Docs | `docs/configuration.md`, `internal/config/sre.go` (doc tag), `CHANGELOG.md` | Families, triggers, detector thresholds, required privileges. |

### Probes (PostgreSQL 14 to 18)

| Probe | Reads | Version handling |
|---|---|---|
| `checkpoint_activity` | timed/requested checkpoints, write/sync ms, buffers, backend writes and fsyncs, WAL bytes/records/FPI, `max_wal_size`, `checkpoint_timeout`, `checkpoint_completion_target` | `pg_stat_bgwriter` (14-16); `pg_stat_checkpointer` + `pg_stat_io` client-backend writes/fsyncs (17+). Avoids the `pg_stat_wal` columns PG18 removed. |
| `temp_file_activity` | this database's `temp_files`/`temp_bytes`, `work_mem`, `hash_mem_multiplier`, `temp_file_limit`, whether `temp_tablespaces` is set | one variant |
| `temp_file_holders` | live temp files by backend (`pg_ls_tmpdir`, pid from the file name) joined to `pg_stat_activity` (state, query_id, age) | needs `pg_monitor` or superuser: otherwise `no_privilege` |
| `temp_spill_statements` | `pg_stat_statements(false)` (no query text) per queryid: calls, temp blocks, block size, stats reset | extension schema resolved at run time; `unsupported` when not installed or not preloaded |
| `replication_lag` v2 | per replica: pid, kind (slot type), write/flush/replay lag seconds, and the lag split into not-sent, sent-not-flushed and flushed-not-replayed bytes | one variant |
| `standby_replay_state` | in recovery, replay paused, receive-minus-replay bytes, last replay age, recovery conflicts, WAL receiver status, `max_standby_streaming_delay`, longest standby query | on a primary, standby fields are NULL |
| `lwlock_waits` | one sample of active client backends and parallel workers grouped by wait event type/event, query_id and database | one variant |

## Families

| Family | Trigger | Plan (probes) | Mechanisms (graph v3) |
|---|---|---|---|
| `checkpoint_storm` | RCA `log_checkpoint_too_frequent`; detector | checkpoint_activity x3 over 6 sample intervals (30 s default) + sage_actions = 4 | `max_wal_size_undersized`, `forced_checkpoints`, `short_checkpoint_timeout`, `checkpoint_write_burst` (amplifies the first two) |
| `temp_file_explosion` | RCA `log_temp_file_created`; detector | temp_file_activity, temp_file_holders, temp_spill_statements x2 + sage_actions = 7 | `runaway_spill_query`, `repeated_spill_statement`, `work_mem_undersized` |
| `replication_lag` | RCA `replication_lag_increasing` (moved from WAL retention), `log_replication_conflict` | replication_lag, standby_replay_state, wal_checkpoint x2 + sage_actions = 7 | `wal_send_backlog`, `standby_flush_backlog`, `standby_replay_backlog` (amplifies the next two), `replay_paused`, `standby_query_delay`, `replication_write_surge` (amplifies the three stages) |
| `lwlock_contention` | detector | lwlock_waits x4 + sage_actions = 5 | `lock_manager_contention`, `subtrans_slru_contention`, `multixact_slru_contention`, `wal_write_contention`, `buffer_contention` |

Every plan reads pg_sage's own actions (CHECK-38) and leaves room for the model turn's one
probe under the 12-probe ceiling (largest plan: 7). Every hypothesis carries a refutation
probe and an operator step (CHECK-37). The model turn works unchanged: node ids come from
the graph, and the graph's facts are rendered on the evidence they cite (tested with a fake
OpenAI-compatible model on an LWLock investigation, and by the bench's fake-model arm on all
four families).

## Product decisions (and why)

1. **Detection thresholds (conservative defaults, `sre.DefaultDetectorConfig`, not exposed
   in YAML yet):** checkpoint storm = 3 or more requested checkpoints within 5 minutes
   that also outnumber timed ones (PostgreSQL warns at one per 30 s); temp-file explosion
   = 1 GiB of temp files in this database within 5 minutes; LWLock contention = 8 or more
   backends on one modeled LWLock class in 3 consecutive polls (45 s at the default poll).
   One investigation per episode (one idempotency key while the condition holds), 30
   minutes between episodes of a family, and never from a reset counter. Waits on LWLocks
   the graph does not model never fire: the investigation could only be inconclusive.
2. **Matcher thresholds:** volume-driven checkpoints carry at least a quarter of
   `max_wal_size` of WAL each (PostgreSQL requests one at about half); runaway spill = 64
   MiB live in one backend and most of the live total; repeated statement = 32 MiB over 2+
   calls between samples and most of the total; work_mem = 3+ spilling statements, none
   dominant, files averaging within 16x `work_mem` (64x rules it out); replication lag =
   16 MiB total for the worst replica, the stage with 50%+ is the mechanism, under 20% is
   ruled out; LWLock = 4+ waiters in at least half of the samples (at least 2 samples).
3. **The detector only polls while `sre.automatic_start` is on**, through the database's
   probe runner (1 probe at a time per database), 3 probes per poll. It keeps its history
   in memory, so a restart forgets it and the first poll after it never fires.
4. **Detector-started investigations have no RCA incident**; their case id is
   `sre:detector:<family>:<database>`, so they appear in the investigations list but not
   linked to a Cases row. Whether the detector should also write `sage.incidents` (and
   notify) is for the coordinator to decide; I kept detection inside the investigator.
5. **`replication_lag_increasing` now starts `replication_lag`, not `wal_retention`.** The
   WAL family reads slots, the archiver and WAL volume, never replica lag.
6. **Operator steps never recommend the taxonomy's dangerous fixes** (global `work_mem`,
   `fsync`/`full_page_writes` off, restarts, promoting a lagging replica, dropping slots);
   a test enforces it. All actions stay manual (L1 proposals at most); nothing executes.
7. **Missing evidence is explicit:** a standby-side mechanism investigated from the
   primary reports `standby_replay_state / not_a_standby`; the upstream stages on a standby
   report `replication_lag / primary_side_unavailable`; no `pg_monitor` reports
   `temp_file_holders / no_privilege`; no `pg_stat_statements` reports
   `temp_spill_statements / extension_not_installed`.

## Spec CHECKs covered

- **CHECK-07** (resets/restarts never read as change): checkpoint, temp and pgss deltas and
  the detector's history (unit tests per family and in the detector).
- **CHECK-37** (refutation probe on every hypothesis): graph tests, per-family diagnosis
  tests, coordinator runs on the real store.
- **CHECK-38** (pg_sage's own action as a hypothesis): every M6 plan and diagnosis;
  verified on persisted hypotheses.
- **CHECK-42** (noise/decoy within 10 points of clean top-1): bench gates per family.
- R1 gates per family on the bench: top-1, abstention, forbidden actions, packet p95.

## PGIncidentBench results

Arms: `causal-graph` (LLM off, gated), `causal-graph+llm` (fake adversarial model; held to
M3-LLM-PARITY and M3-LLM-ROOT), and the derived `rules-only` and `always-escalate`
baselines. One repeat per server. Rates are hits/denominator.

**causal-graph arm, per family (PG17 = own `pgsage-ag3`; PG14 and PG18 = shared matrix):**

| Family | Server | Safe Pass | top-1 (pos+noise) | noise top-1 | decoy false-root rate | abstain (decoy+benign) | gates |
|---|---|---|---|---|---|---|---|
| checkpoint_storm | 17 / 14 / 18 | 5/5 / 5/5 / 5/5 | 3/3 / 3/3 / 3/3 | 1/1 each | 0/1 each | 2/2 each | all pass |
| temp_file_explosion | 17 / 14 / 18 | 7/7 each | 4/4 each | 1/1 each | 0/2 each | 3/3 each | all pass |
| replication_lag | 17 / 14 / 18 | 5/5 each | 3/3 each | 1/1 each | 0/1 each | 2/2 each | all pass |
| lwlock_contention | 17 / 14 / 18 | 5/5 each | 3/3 each | 1/1 each | 0/1 each | 2/2 each | all pass |

Gates per family on every server: R1-TOP1, R1-ABSTAIN, R1-FORBIDDEN (0 forbidden actions),
CHECK-42-NOISE, CHECK-42-DECOY and R1-PACKET-P95 pass (p95 packet: checkpoint 24.6 s,
LWLock 12.2 s, replication 8.7 s, temp 5.6 s on PG17). The LLM-on arm with the fake model
matched the graph on every run (M3-LLM-PARITY and M3-LLM-ROOT pass: 0 conclusive roots
changed). Interval caveat: with 3 to 7 runs per family, the 95% Wilson intervals are wide
(for example top-1 3/3 is [44-100]); the gates use point estimates, as the bench always has.

The **full PG17 bench (all 9 families, both live arms)** passed twice: once alone (812 s,
52 scored runs per live arm, 100% Safe Pass) and once inside the full suite. Rules-only, for
contrast, on PG17: checkpoint 4/5 Safe Pass (calls forced checkpoints "max_wal_size"),
temp-file 2/7 Safe Pass with 2/2 decoy false roots (reads the cumulative counter and any
live temp file as a runaway), replication 4/5 (calls a flush backlog "replay"), LWLock 5/5.

**PG14/PG18 notes.** The first matrix run exposed three environment problems, fixed and
rerun: (1) a concurrent bench on the shared PG18 server created `bench_slot_` slots, which
the safety grader counted as this run's (slot names now carry a per-process random prefix);
(2) on PG14 the statistics collector delivered the decoy's own setup `CHECKPOINT` late, so
every quiet run looked contaminated (the program now waits until it is counted); (3) on
PG14 another session's `CHECKPOINT` commands once made the undersized run look forced (the
program now repeats a run whose requests were not volume-driven). After the fixes, PG14 and
PG18 pass with no retries and no skips. No family is unavailable on any version: the views
that differ (`pg_stat_bgwriter` vs `pg_stat_checkpointer`/`pg_stat_io`, `pg_stat_wal`
columns removed in 18) have per-version variants.

Per-scenario outcomes (PG17, causal-graph arm): every clean and noise run named the gold
root (with the gold contributing factor where there is one: `checkpoint_write_burst`,
`replication_write_surge`); every decoy and benign run was inconclusive.

## Test Results

**Commands** (Docker `golang:1.25`, repo root mounted):
`go test -count=1 -cover -timeout 60m ./...` against PG17 (`pgsage-ag3`);
`go test -count=1 -cover ./internal/sre/... ./internal/config/` and `-run SRE ./cmd/pg_sage_sidecar/`
against PG14 (:55414) and PG18 (:55418); `go test -race -count=1 ./internal/sre/...` against
PG17; the bench as above; `golangci-lint run ./...`.

**Totals (PG17 full suite):** 68 packages with tests. First pass: 62 ok, 6 failed. One was a
real bug of mine (`cmd/gen_config_meta`: the new `sre.automatic_start` doc tag was 305
characters, over the 200 limit; fixed in `fix(config)`). Five were load or timing failures
on the shared machine (fixture `DROP DATABASE` timeouts in `internal/advisor`,
`internal/explain`, `internal/ha`; `TestApplyLockCeilingCapsCustodian`; lease-timing tests in
`internal/sre`). All six pass on rerun (`go test -count=1 -cover` of those packages: all ok).
Lint: 0 issues.

**Coverage of touched packages (PG17):** `internal/sre` 88.3%, `internal/sre/causal` 94.4%,
`internal/sre/probes` 91.5%, `sre-bench` 88.9%, `internal/config` 87.5%,
`cmd/pg_sage_sidecar` 71.9%, `cmd/gen_config_meta` 86.4%. All packages meet coverage
thresholds (70% business, 50% utilities).

**Matrix (touched packages):** `internal/sre/causal` 94.4%, `internal/sre/probes` 90.9% (PG14)
/ 91.1% (PG18), `internal/config` and the SRE tests of `cmd/pg_sage_sidecar` pass on both.
`internal/sre` fails only lease-timing tests on the loaded shared servers
(`TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep`,
`TestModelTurn_SlowCallKeepsTheLease`, `TestStore_PendingFindsQueuedAndOrphanedWork`,
`TestReserveModel_CrashAfterReservationKeepsTheHold`, `TestStore_ConcludeNeedsTheCurrentLease`),
which pass or fail from run to run; the same tests fail the same way at the base commit
`e44bd6b` on PG14. Every M6 test passes on PG14 and PG18.

**Race (`-race`, PG17):** no data race in `internal/sre/...` after fixing one in my own test
(`TestCatalog_LWLockWaitsSamplesActiveBackends` released a connection its goroutine still
used). `TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep` (300 ms lease) fails
under the race detector's slowdown, as at the base commit.

**Skipped tests:** none in the touched packages (`go test -v` of `internal/sre/probes`,
`internal/sre/causal`, `internal/config` and the SRE tests of `cmd/pg_sage_sidecar` report 0
SKIP on PG17). The full suite ran without `-v`, so skips in untouched packages were not
counted. Bench scenarios skipped: `wal-archiver-failure` (pre-existing; `archive_mode` is
off on these servers); no M6 scenario was skipped on 14, 17 or 18.

**Failures:** none remaining in M6 code. See above for the pre-existing lease-timing tests.

**Coverage gaps:** none below threshold.

## Bugs found this session

1. [BUG, fixed before merge] the first temp/LWLock bench runs: an LWLock storm driven by
   client round trips is bursty (most sessions idle between statements), so sustained
   contention showed in under half the samples; the fault programs now loop server-side.
   The matcher's sustained-sample rule was right; the fixture was wrong.
2. [BUG, fixed] `TestModelTurn_RanksAnM6Family` cited an evidence entry without the number
   it quoted; the claim validator rightly rejected it (graph facts are rendered on the
   evidence they cite, the last sample).
3. [LIMITATION] the "WAL not leaving the primary" stage cannot be produced in the Docker
   fixture: a consumer that stops reading behind Docker Desktop's port proxy does not
   back-pressure the walsender (the proxy buffers what it sends). The scenario was removed;
   the matcher is unit-tested.
4. [BUG, fixed] the `sre.automatic_start` doc tag I extended was 305 characters and broke
   `gen_config_meta` (CHECK-T16, 200 max); shortened and `config_meta.json` regenerated.
5. [BUG, fixed; pre-existing harness] the bench's safety grader counted every
   `bench_slot_` slot on the server, so another bench process on a shared server made
   read-only runs look unsafe. Slot names now carry a random per-process prefix.
6. [BUG, fixed] on PG14 the checkpoint decoys' own setup `CHECKPOINT` was counted late by
   the statistics collector and contaminated every attempt.
7. [BUG, fixed] a data race in my probe test's cleanup (connection released while in use).
8. [PRE-EXISTING, not M6] lease-timing tests (`LeaseTTL` 200-300 ms or 1 s) fail
   intermittently on the loaded shared matrix servers
   (`TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep`,
   `TestModelTurn_SlowCallKeepsTheLease`, `TestStore_*Lease*`). The same tests fail the
   same way at the base commit `e44bd6b` on PG14 under the same load; they pass on an idle
   server.

## Post-test audit

- **Inputs not tested end to end:** standby-side mechanisms (`replay_paused`,
  `standby_query_delay`, the standby's replay backlog), `wal_send_backlog`,
  `short_checkpoint_timeout`, and the subtransaction, multixact and buffer LWLock classes
  have unit tests only (no standby in the fixture; the class tests are synthetic samples).
  `pg_ls_tmpdir` does not list parallel-query fileset directories or temp tablespaces other
  than the default (the latter is reported as an observed fact). The detector runs end to
  end only for LWLock contention (composed integration test); checkpoint and temp detection
  are unit-tested with scripted probes.
- **Assertions that could pass when broken:** mutation testing found six surviving mutants
  in the matchers (dominance support, the WAL-rate prediction bound, the pg_stat_database
  contradiction, the forced-checkpoint frequency bonus, minority queryid attribution);
  each now has a failing test. 13 more mutants (thresholds, boundaries, cooldown, reset
  handling, combined-source errors, the checkpoint plan's wait, extension schema quoting)
  are killed.
- **Fakes that hide failures:** the coordinator tests' scripted runner stamps samples with
  `time.Now()`, so rates in those tests are huge; rate-dependent outcomes are covered by the
  causal unit tests (realistic spans) and the bench on real PostgreSQL. The bench's replicas
  are logical consumers, not physical standbys.

## What is left

- A physical-standby fixture (two containers) to exercise standby-side mechanisms and a
  network-shaped send backlog (Toxiproxy or tc-netem).
- Fault programs for subtransaction SLRU (SAVEPOINT loop + long transaction), multixact
  and buffer contention, and a short `checkpoint_timeout`.
- Exposing the detector thresholds in `sre.*` config, and deciding whether detector
  episodes should create `sage.incidents` (notifications, Cases link).
- Bench runtime: a full repeat now takes about 14 minutes for both live arms; CI may want
  `SAGE_BENCH_FAMILIES` splits.
