# Sage SRE M6 part 2: runway families and pre-incident investigations

Date: 2026-10-02. Branch `claude/sre-m6-runways`, based on e44bd6b. Spec:
`reviews/2026-09-26/AI-SRE-SPEC.md` §4 (R2), §12 and the M6 row of §13. The reactive R2
families (checkpoint storm, temp files, replication lag, LWLock) are on
`claude/sre-m6-reactive` and are not part of this branch.

## What was built

1. **Runway probes** (`internal/sre/probes/catalog_runway.go`, `decode_runway.go`). They
   are read-only catalog probes with typed status:
   - `xid_runway`: XID and multixact age against the warning limit (2^31 - 1 - 40M), and
     autovacuum workers busy or free.
   - `wraparound_tables`: each table's age against its effective freeze maximum (the lower
     of the setting and the reloption).
   - `xmin_horizon`: who holds the horizon (session, standby, prepared transaction,
     slot, catalog slot), with ages.
   - `autovacuum_cancellations`: counted from `sage.incidents` over a window.
   - `wal_runway`: WAL position, configured WAL limits, database bytes.
   - `wal_directory`: `pg_wal` size and `.ready` segments. It needs `pg_monitor`.
   - `sequence_runway`: ascending sequences with their binding limit: the sequence type,
     the owning column's type, or an explicit `MAXVALUE`.
   - `runway_trends`: the regression slope, r² and span of each sampled series in its
     current epoch.
2. **`sage.runway_samples`**. This is a new idempotent migration, registered last in
   `bootstrap.go`. `schema_version` is not bumped. When a counter goes down (a restart,
   failover or reset), a new epoch starts, so a trend never spans a reset.
3. **Causal graph v3** (`internal/sre/causal/graph_v3.go`, `wraparound*.go`, `diskwal.go`,
   `sequence.go`). There are 11 new nodes in three families:
   - `wraparound_runway`: horizon held by a session, by a prepared transaction or by
     replication; autovacuum saturated, disabled or cancelled; and an XID consumption
     surge, which amplifies the others.
   - `disk_wal_runway`: reuses the WAL nodes (inactive slot, slow consumer, archiver,
     write surge), measured by trend, and adds `database_growth`.
   - `sequence_runway`: sequence type limit, a column narrower than its sequence, and an
     explicit `MAXVALUE`.
   Every node has a refutation probe (CHECK-37). Every runway plan runs `sage_actions`, so
   pg_sage's own change is always a hypothesis (CHECK-38).
4. **Runway investigations** (`internal/sre/runway.go`). Each kind has a plan (4 to 9
   probes, two samples, within the probe ceiling, with two probes left for the model). A
   concluded runway investigation lists **custodian proposals** in `Summary.Proposals`:
   the standing freeze or WAL-bound proposal, with the policy gate's verdict. It is
   explained, never recorded or executed. If the advisor fails, the investigation still
   concludes and logs a warning.
5. **Runway monitor** (`internal/runway/`). On its own ticker (60 s by default) it does
   the following:
   - samples the series;
   - prunes old samples;
   - evaluates each runway against its horizon;
   - upserts or resolves `forecast_wraparound_runway`, `forecast_wal_runway` and
     `forecast_sequence_runway` findings, which become Cases of the forecast type;
   - starts one investigation per finding and severity, keyed by idempotency key
     `runway:<finding>:<severity>`.
   A standby writes nothing. The monitor and the custodian advisor are wired into every
   database runtime (`cmd/pg_sage_sidecar/database_runtime_runway.go`,
   `sre_runway_advisor.go`).
6. **Measured `disk_full_slot` credit**
   (`internal/executor/custodian_wal_incident.go`, `internal/runway/credit.go`). A
   verified custodian WAL bound credits a `near_miss` through
   `value.Service.CreditAvoidedIncident` only when all of these hold:
   - the server is self-managed;
   - disk capacity is declared;
   - the measured disk-usage trend before the action reached capacity inside the horizon
     (minimum samples and span, rising, r² ≥ 0.5);
   - after the action, usage plus the WAL the bound still allows plus measured database
     growth stays under capacity.
   The LEDGER entry is updated.
7. **Model-turn compatibility.** The model turn is generic over graph nodes and
   evidence. New tests show the following on runway investigations:
   - the model sees the family and `causal-v3`;
   - it ranks the family's open hypotheses and cites runway evidence;
   - while the graph is inconclusive, it may ask for `runway_trends` with a typed window;
   - another family's node is out of scope and is repaired;
   - a conclusive graph root wins over the model.
8. **Configuration** `sre.runways.*` (`internal/config/runway.go`), regenerated config
   metadata, `docs/configuration.md`, a CHANGELOG entry, the LEDGER, and the bench README.
9. **PGIncidentBench runway scenarios** (`sre-bench/scenarios_{wrap,diskwal,seq}.go`,
   `env_runway.go`, `rules_runway.go`). There are 22 scenarios, and every family has
   clean, noise, decoy and benign variants. Each scenario builds its trend with the real
   runway sampler on a compressed timescale.

## Product decisions (recorded in `reviews/decisions/LEDGER.md`)

- **On by default, independent of `sre.automatic_start`.** Runway sampling and
  pre-incident investigations are read-only and bounded. The spec calls them the safest
  autonomy. R1 reactive auto-start stays as configured.
- **Conservative horizons:**
  - wraparound: 14 days to the warning limit, critical at 72 h; a table is overdue at 1.25
    times its freeze maximum, critical at 2 times;
  - disk and slot: 72 h, critical at 24 h;
  - sequences: 30 days, critical at 7 days;
  - a projection needs 10 samples over 30 min in a 6 h lookback, and disk and slot
    projections need r² ≥ 0.5.
- **Limits are measured or declared, never guessed:**
  - disk capacity is only `forecaster.disk_capacity_bytes`;
  - a slot's limit is `max_slot_wal_keep_size`, or else the WAL custodian's 10 GiB
    backstop;
  - a sequence's limit is the lower of its maximum and its owning column's maximum.
- **Mechanisms are blamed only near a limit.** A long transaction or busy autovacuum is
  supported only while a table is at half its freeze maximum or the runway is inside the
  horizon. A sequence is blamed only while it is consuming.
- **No new execution.** Proposals reuse the existing custodians through `policy.Gate`
  (explain only). Sequence widening rewrites the table, so it stays a manual, reviewed
  step: irreversible classes never exceed L1.
- **Credit only where measurable.** Managed providers are never credited: free space is
  not visible from SQL there, and storage may autoscale. Without `pg_monitor`, nothing is
  credited.
- **Runway settings are YAML-only** (restart lifecycle), like the other Sage SRE settings.

## Spec checks covered

| Check | How |
|---|---|
| R2 families: wraparound / autovacuum starvation, disk/WAL runway, sequence exhaustion | graph v3 + probes + bench, PG14–18 |
| R2 pre-incident investigations | runway monitor → forecast finding → investigation (`monitor_e2e_db_test.go` opens a real one) |
| CHECK-37 refutation probe per hypothesis | every v3 node names a catalog refutation probe; `graph_test.go` checks each against the catalog |
| CHECK-38 pg_sage's own action is a hypothesis | `sage_actions` in every runway plan; `WithSelfActions` |
| CHECK-42 noise/decoy within 10 points of clean | bench gates per runway family: pass |
| R1 gates applied per family (top-1, abstention, forbidden, packet p95) | bench: pass for all three runway families |
| Model turn (M3) on new families | `model_runway_db_test.go`; bench M3-LLM-PARITY and M3-LLM-ROOT pass |
| Avoided-incident credit only when measured | `credit_test.go`, `custodian_wal_incident_integration_test.go` |

## Bench results (PG17, 1 repeat, `causal-graph` = gated deterministic arm)

| family | Safe Pass | top-1 | clean top-1 | noise top-1 | decoy false dx | abstain (insufficient) | packet p95 |
|---|---|---|---|---|---|---|---|
| wraparound_runway | 100% (6/6) [61-100] | 100% (4/4) [51-100] | 100% (3/3) | 100% (1/1) | 0% (0/1) | 100% (2/2) | 3.1 s |
| disk_wal_runway | 100% (8/8) [68-100] | 100% (5/5) [57-100] | 100% (3/3) | 100% (2/2) | 0% (0/2) | 100% (3/3) | 3.8 s |
| sequence_runway | 100% (8/8) [68-100] | 100% (5/5) [57-100] | 100% (3/3) | 100% (2/2) | 0% (0/2) | 100% (3/3) | 5.4 s |
| all families (52 scored, 1 skipped) | 100% (52/52) [93-100] | 100% (34/34) [90-100] | 100% (22/22) | 100% (12/12) | 0% (0/11) | 100% (18/18) | 5.4 s |

These are the baselines on the same evidence:

- **`rules-only`** is a naive first-match rule list.
  - It makes 4 false diagnoses on runway decoys: `disk-churn`, `disk-slot-keeping-up`,
    `seq-dormant-near-limit` and `seq-cycling-near-limit`.
  - It misses `wrap-xid-surge`.
  - The result is Safe Pass 75% (6/8) for disk, 75% (6/8) for sequence, and 100% (6/6)
    for wraparound, with top-1 75% (3/4) on wraparound.
- **`always-escalate`** has 100% Safe Pass and 0% top-1 everywhere.
- **`causal-graph+llm` with the fake adversarial model** shows parity on all three
  runway families. It changed 0 conclusive roots: 0 of 4 on wraparound, 0 of 5 on disk
  and 0 of 5 on sequence.

Per-scenario outcomes (all correct for `causal-graph`):

- **Wraparound:**
  - session holder, prepared holder and XID surge (positive);
  - session holder under load (noise);
  - an idle transaction holding no horizon (decoy, inconclusive);
  - a healthy cluster (benign, inconclusive).
- **Disk/WAL:**
  - inactive slot, slow consumer and database growth (positive);
  - inactive slot and growth under load (noise);
  - load-and-truncate churn and a consumer that keeps up (decoys, inconclusive);
  - steady (benign).
- **Sequence:**
  - type limit, narrow owning column and explicit MAXVALUE (positive);
  - two of those under load (noise);
  - a dormant and a cycling sequence near their limit (decoys, inconclusive);
  - a sequence far from its limit (benign).

The skipped scenario is the pre-existing `wal-archiver-failure`, because the archive
fixture is off.

**PG14 and PG18 (shared matrix servers, 1 repeat, `causal-graph`):**

| family | PG14 Safe Pass | PG14 top-1 | PG18 Safe Pass | PG18 top-1 | decoy false dx (14 / 18) |
|---|---|---|---|---|---|
| wraparound_runway | 100% (5/5) | 100% (3/3) | 100% (4/4) | 100% (2/2) | 0/1 / 0/1 |
| disk_wal_runway | 100% (8/8) | 100% (5/5) | 100% (8/8) | 100% (5/5) | 0/2 / 0/2 |
| sequence_runway | 100% (8/8) | 100% (5/5) | 100% (8/8) | 100% (5/5) | 0/2 / 0/2 |

The `-race` run on PG17 had the same results as the plain run: 52/52 Safe Pass and 34/34
top-1. No run on any server produced a wrong root or a false diagnosis.

On both matrix servers the bench test itself fails, because some runs were not scored.
Their premise did not hold while other agents' suites used the same servers, and the
contamination guards refused to score them:

- **Skipped runs:** `wrap-prepared-holder` is skipped on both servers, because
  `max_prepared_transactions` is 0.
- **PG18:**
  - other sessions burned 605 to 4263 XIDs/s during wraparound runs;
  - other databases changed by 8 MB during a disk run;
  - so `wrap-xid-surge` errored after 3 attempts for `causal-graph`.
- **PG14:** the logical consumer of `disk-slot-keeping-up` did not confirm within 15 s
  in the LLM arm's run. The pre-existing `wal-slot-keeping-up` fails the same way under
  load.

## Bugs found

1. **[BUG] `causal/diskwal.go`: immaterial slot growth named `slow_consumer`.** A
   consumer that keeps up drifts by a few KB per sample, and a steady r² made that look
   like a cause. The test was committed failing (b1eaa9a). The fix (75b6554) requires at
   least 1 MiB of growth over the trend's span, as database growth already did.
2. **[BUG] `retention/cleanup.go`: `sage.runway_samples` was not accounted for.** The
   coverage check failed. It is now listed as pruned by the runway monitor
   (`sre.runways.sample_retention_hours`) (bfa3583).
3. **[BUG] `store` config consistency: the 13 `sre.runways.*` keys were not classified.**
   They are now YAML-only, like the other Sage SRE settings (cb5952a).
4. **[BUG, bench] The fixed 8-minute bench budget expired before the runway scenarios
   ran.** The budget is now 20 s per scenario per repeat (ae7f2f0).
5. **[FINDING] `forecaster.disk_capacity_bytes` is documented as "0 = auto-detect", but
   nothing auto-detects.** The runway monitor treats 0 as undeclared: there is no disk
   runway and no credit. The doc string is left unchanged on this branch.
6. **[FINDING] `forecaster.ForecastGrowth` is still not called anywhere.** The runway
   monitor does not depend on it.
7. **[TEST LOGIC] Fixed with reasons in their commits:**
   - two probe checks: quoted identifiers, and a non-transactional WAL message that was
     not flushed (4e4573b);
   - two model-turn fixtures: a helper name collision, and a disagreement test with no
     second open hypothesis (8e2f70f);
   - one arithmetic error in a measurement test.

## Test Results

**Command:** `go test -count=1 -cover -v -timeout 40m ./...` (from `sidecar/`, Docker
golang:1.25, `SAGE_TEST_DATABASE_URL` = own PG17 :55476, `GEMINI_API_KEY` empty).

**Total (full suite, PG17):** 8999 passed, 14 failed, 12 skipped.

The full suite ran at the same time as the PG14 and PG18 `-race` runs and other
agents' suites. The 14 failures were in three groups:

- **Three real bugs of this branch.** These were the retention and config-consistency
  checks, which are now fixed: `TestRetentionRules_CoverEveryTimeSeriesTable` and
  `TestConfigConsistency_AllowedKeysMatchStruct`.
- **Timing under load:**
  - `cmd`: `TestComposedSRE_MetaDBCollectorIncidentNotify` (migration timeout) and
    `TestFleetMCPGetValueAggregates` (DROP DATABASE timeout);
  - `executor`: `TestApplyLockCeilingCapsOperatorAnalyze`;
  - `value`: `TestCreditAvoidedIncidentReportsLostConnection`;
  - `probes`: `TestCatalog_WALCheckpointAndVacuumViews`;
  - `sre`: 7 tests built on 300 ms leases or model timeouts.
- **Reruns on PG17 after the fixes:**
  - `retention`, `store`, `executor`, `value`, `cmd/pg_sage_sidecar`, `schema`,
    `sre/probes`, `runway`, `config`, `autonomy`, `sre/causal` and `sre` all pass;
  - `TestRunner_SidecarWideLimit` failed once under load, then passed on rerun.

The `sre-bench` package passed inside the full suite: 911 s, 88.6% coverage.

**Coverage (touched packages, PG17):**

| package | coverage | floor |
|---|---|---|
| internal/sre | 88.2% | 70% |
| internal/sre/causal | 95.0% | 70% |
| internal/sre/probes | 92.4% | 70% |
| internal/runway | 89.1% | 70% |
| internal/schema | 81.6% | 70% |
| internal/config | 87.7% | 70% |
| internal/executor | 83.0% | 70% |
| internal/autonomy | 79.8% | 70% |
| internal/retention | 97.6% | 70% |
| internal/store | 74.2% | 70% |
| internal/value | 95.3% | 70% |
| cmd/pg_sage_sidecar | 72.4% | 70% |
| sre-bench | 88.6% | 70% |

**PG14 / PG18** (`go test -p 2 -count=1 -cover` on the touched packages, shared matrix
servers):

- **PG14:** every touched package passes except `sre-bench`, which has one unscored
  run under load (see the bench section). Coverage on PG14:
  - `sre` 88.3%, `sre/causal` 95.0%, `sre/probes` 91.8%;
  - `schema` 81.6%, `runway` 89.1%, `executor` 83.0%, `autonomy` 80.4%;
  - `retention` 100%, `store` 74.2%, `cmd` 72.4%.
- **PG18:** every package passes except these three:
  - `sre-bench` has unscored, contaminated runs;
  - `sre`: `TestCoordinator_ResumesAnOrphanedInvestigationAtTheNextStep` fails, a
    300 ms lease test. It also fails 1 in 4 on base e44bd6b against PG18, and passes 4/4
    on this branch on rerun;
  - `sre/probes`: `TestCatalog_WALRunwayReadsPositionAndSettings` hit the 500 ms probe
    deadline under load, then passed 3/3 on rerun in 0.11 s.

**-race (PG17)** (`go test -race -p 2 -count=1 -cover` on the touched packages):

- There are no data races.
- Every package passes, including `sre-bench`: 681 s, 52/52 Safe Pass.
- In `sre`, two timing tests failed while the bench ran beside them:
  `TestModelTurn_SlowCallKeepsTheLease` and
  `TestReserveModel_CrashAfterReservationKeepsTheHold`. Both pass 3/3 alone with
  `-race`, on base and on this branch.
- `sre` alone with `-race` passes at 88.1%.

**Lint:** `golangci-lint run ./...` reports 0 issues. Every new or changed function is
≤ 50 lines, every new file ≤ 500 lines, and every new line ≤ 100 columns. The exception is
`doc:` struct tags in `internal/config/runway.go`, which follow the existing config
convention (`sre.go` has the same).

### Skipped Tests (must be zero or justified)

12 skips in the full suite. All are pre-existing tests that need a live cloud or LLM
provider or another OS:

- AWS RDS, Cloud SQL, Lakebase and Azure live provisioning and parameters: 7;
- live LLM: 2 (`TestChatWithToolsLive_RealProvider`, `TestTier2Live_RealGemini`);
- the Windows log-dir test;
- the RCA child-process fixture.

None of them is in a package this branch adds. There are no skips in the runway tests.
Inside the bench, `wal-archiver-failure` is skipped because `archive_mode` is off.

### Failures

None after the fixes, apart from timing tests that already fail without this branch:

- `TestStore_PendingFindsQueuedAndOrphanedWork` fails on base e44bd6b too.
- `TestStore_StaleWorkerCannotCommit` flakes 1 in 3 on this branch with a 300 ms lease.

Both are outside the runway code.

### Coverage Gaps (packages below threshold)

None. All packages meet coverage thresholds (lowest: `cmd/pg_sage_sidecar` at 72.4% and
`internal/store` at 74.2%).

### Bugs Found This Session

1. [BUG] `causal/diskwal.go` `steadySlotGrowth`: immaterial slot growth named a slow
   consumer. Fixed.
2. [BUG] `retention/cleanup.go`: `runway_samples` was missing from retention coverage.
   Fixed.
3. [BUG] `store` config consistency: the runway keys were unclassified. Fixed.
4. [BUG] `sre-bench/bench_test.go`: the fixed pass budget was too small for the scenario
   set. Fixed.

### Manual Checks Remaining

- MANUAL: CI runtime. The bench now takes about 10 min alone, and 15 min inside a
  parallel `go test ./...`. That is over the integration job's `-timeout 600s` and the
  unit job's `900s` (see coordinator decisions).
- MANUAL: managed-provider behaviour (RDS, Cloud SQL) of the disk runway. It is designed
  to stay silent without declared capacity and to never credit, but it was not exercised
  on a real provider.

## Post-test audit

1. **What input would break this that I haven't tested?**
   - A cluster with thousands of databases. `database_bytes` sums `pg_database_size`
     over every connectable database on every pass, and in fleet mode every runtime does
     this. The probe's 500 ms timeout bounds it, and a timed-out sample is skipped, but
     the cost is quadratic in fleet size.
   - Descending sequences are out of scope by design.
   - Multixact runway is covered by the decoder and graph tests, but has no live
     scenario: burning multixacts fast is impractical in a bench.
2. **What behavior is not covered by any assertion?**
   - The Markdown and JSON bench output is logged, not asserted (the report unit tests
     cover rendering).
   - The monitor's log lines on probe failure are not asserted.
3. **Are there assertions that would pass even if the feature was broken?**
   - Mutation testing on the thresholds closed four gaps (9f9fe3e): surge ratio and
     floor, sub-1 MiB growth, and the oldest-holder bonus.
   - Two surviving mutants are equivalent:
     - the monitor's `loaded` reload guard, where reloading again is idempotent;
     - the executor reusing the after-measure as the baseline when the before-measure
       is missing, which the credit refuses anyway.
4. **Are there any test doubles that hide real failure modes?**
   - The coordinator tests use scripted probe runners. Each family also has a live test
     on real PostgreSQL (`causal_runway_db_test.go`, `runway_db_test.go`,
     `monitor_e2e_db_test.go`), and the bench injects real faults.
   - The WAL credit integration test runs a real `ALTER SYSTEM` bound.
   - The custodian advisor is tested with fakes. Its gate path is
     `policy.Explainer`, which is tested in `custodian_explain_test.go` against the real
     policy.

## What's left

- The autoscaling cooldown horizon (RDS) from the R2 list. It needs provider storage
  metrics, which `providerobs` does not expose.
- Disk capacity auto-detection, or a correction of the "0 = auto-detect" doc string.
- Per-cluster de-duplication of `database_bytes` sampling in fleet mode.
- A live multixact scenario, and repeats ≥ 3 for run-to-run consistency on runway
  families. This run had 1 repeat, so consistency shows n/a.
- The forecaster's `ForecastGrowth` path stays unwired.

## Coordinator decisions

1. **Causal graph version.** This branch bumps `GraphVersion` to `causal-v3` (25 nodes).
   `claude/sre-m6-reactive` probably bumps it too. Merge both node sets under one
   version.
2. **Shared switch edits.** These are small additive cases that will conflict textually
   with M6-reactive: `sre/request.go` `triggerKinds`, `sre/plan.go` `diagnose` and
   `seriesProbes`, `probes/registry.go` `knownFamilies`/`catalogSpecs`,
   `sre-bench/rules.go` `Derive`, and `sre-bench/scenarios_lock.go` `Scenarios()`.
3. **Proposals live in `Summary.Proposals`** (JSON), not in a new event type. This avoids
   the `sre_events` CHECK constraint that M5's `propose_action` work may change. If M5
   adds a proposal event, runway proposals could move there.
4. **Runways default on, independent of `sre.automatic_start`.** See the LEDGER.
5. **CI bench time.** PGIncidentBench runs inside every `go test ./...` job. With the
   runway scenarios it takes about 10 min alone, and 15 min when run with every package
   in parallel. That is over the 600 s integration timeout, and M6-reactive adds more
   scenarios. Recommendation: run the bench in its own CI step (`-run TestPGIncidentBench
   -timeout 30m`) and skip it in the `./...` jobs. `ci.yml` is not changed on this branch.

## Commits

```
3129d83 test(sre): split runway tests longer than 50 lines
cb5952a test(store): list the runway settings as YAML-only
bfa3583 fix(retention): account for the runway samples table
ae7f2f0 test(bench): budget the bench per scenario
1d728fb feat(bench): add runway fault programs and rules-only baselines
8e2f70f test(sre): fix two logic errors in the runway model-turn tests
22b2aff docs(sre): document runways, their horizons and the disk-full credit
bd53963 feat(runway): expose one sampling pass without evaluation
6828e47 test(sre): specify the model turn on runway investigations
75b6554 fix(sre): require material growth before a slot trend names a cause
b1eaa9a test(sre): immaterial slot growth is not a slow consumer
4615a59 test(bench): specify runway scenarios, baselines and the sampler hook
188ed83 test(runway): open a real pre-incident investigation end to end
f510f54 feat(sre): wire the runway monitor and the custodian advisor into every runtime
c6b57ea feat(executor): credit a measured disk-full near miss for a WAL bound
288e03b feat(runway): sample runways and open pre-incident investigations
6b9f625 feat(sre): run pre-incident runway investigations with custodian proposals
6e2245a test(sre): specify runway investigations, the runway monitor and WAL credit
9f9fe3e test(sre): close mutation gaps in the runway families
18e1747 feat(sre): add the runway families to the causal graph (v3)
722ff91 test(sre): specify the runway families of the causal graph
2e9c713 feat(sre): add the runway probes and the runway samples table
4e4573b test(sre): fix two logic errors in the runway probe checks
355ef2f test(sre): specify the runway probes and the runway samples table
(plus this report)
```
