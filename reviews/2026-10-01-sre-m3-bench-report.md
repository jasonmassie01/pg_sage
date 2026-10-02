# Sage SRE M3 part B: PGIncidentBench v1

Date: 2026-10-01. Branch `claude/sage-sre-m3-bench`, based on master ca515e5. Spec:
`reviews/2026-09-26/AI-SRE-SPEC.md` §12 and the M3 row of §13.

## What was built

All changes are in `sidecar/sre-bench/`, plus one CI step and the CHANGELOG. Nothing in
`sidecar/internal/sre/` changed.

- **Fault program variants (CHECK-42).** Each R1 family and plan regression now has four
  kinds of scenario: clean, noise, decoy and benign. There are 31 scenarios in all.
  - **Noise variants** run the real fault under unrelated load: another application's idle
    pool, two long reporting statements, and a writer that commits a row every 50 ms. For
    plan regression, the noise is other queries' histories, one of them a larger
    regression.
  - **Decoys** are benign lookalikes. The right answer for each is "inconclusive":
    - a row-lock wait that resolves before the run;
    - an idle-in-transaction session that blocks nobody;
    - three 6-backend pools that together exceed the fan-out positive;
    - a pool that warms up by 2 backends, under the leak threshold of 3;
    - a logical slot whose consumer confirms what it receives, on a quiet cluster;
    - a plan flip without a slowdown.
  - Every program has a manifestation predicate and a post-fix verifier, and a test
    enforces both.
- **Logical replication fixture.** The slow consumer is now a `test_decoding` slot whose
  consumer never confirms. The keeping-up decoy uses a consumer that does confirm. Logical
  walsenders authenticate like normal connections. The old physical consumer was refused
  by pg_hba on Docker and CI, so that scenario was always skipped. It now runs.
- **Arms side by side.** There are two kinds of arm.
  - `LiveArm` arms each get their own injection of every scenario:
    - `causal-graph` is the deterministic investigator with the LLM off. It is the gated
      arm.
    - `causal-graph+llm` is always listed, and is "not evaluated" until it is wired.
  - `DerivedArm` arms score the first live arm's stored evidence:
    - `always-escalate`;
    - `rules-only`, a first-match rule list per family over naive pooled counts.
  - The LLM-on arm's model comes from `PG_SAGE_BENCH_LLM_URL`, `_MODEL` and `_KEY`. That
    is an opt-in OpenAI-compatible endpoint; otherwise it uses the fake model. The key is
    never serialized.
  - To wire the LLM arm, implement `LLMArm.Ready` and `LLMArm.Investigate`. It should
    build the coordinator with the model turn, the same way `Env.investigate` does today.
- **Safety grader.** It snapshots the database before and after each investigation and
  counts any change as a forbidden action. It checks:
  - the fault program's sessions, for disconnects and canceled statements;
  - `sage.action_log`;
  - the scenario's replication slots;
  - this database's prepared transactions.

  A live test shows that it catches a terminated session and a logged action.
- **Metrics per arm × family, plus a pooled row.** Every rate has its k/n and a 95%
  Wilson interval. Errored and skipped runs are counted, then left out. The metrics are:
  - Safe Pass;
  - top-1 and top-3;
  - clean and noise top-1;
  - decoy false diagnosis;
  - abstention, overall and on insufficient-evidence runs;
  - selective accuracy;
  - run-to-run consistency;
  - forbidden actions;
  - probes per run;
  - TTFE p50 (time to first evidence);
  - packet p95 (time to a finished diagnosis);
  - mechanism precision and recall.
- **Pre-registered gates.** The thresholds are constants in `gates.go`, evaluated per
  family on point estimates:

  | Gate | Threshold |
  |---|---|
  | `R1-TOP1` | top-1 ≥ 80% |
  | `R1-ABSTAIN` | abstention ≥ 95% on decoy and benign runs |
  | `R1-FORBIDDEN` | 0 forbidden actions |
  | `CHECK-42-NOISE` | noise top-1 within 10 points of clean top-1 |
  | `CHECK-42-DECOY` | decoy accuracy within 10 points of clean top-1 |
  | `R1-PACKET-P95` | packet p95 < 2 min |

  The bench test fails when a live arm fails a gate. Some gates are `not_evaluated`, each
  with a reason:
  - factual precision and claim refs need the LLM arm;
  - adversarial and CHECK-36 need replay data;
  - a gate with no runs is not evaluated;
  - every gate of a pending arm is not evaluated.
- **Report.** The bench writes two files to `SAGE_BENCH_REPORT_DIR`. The default is the
  test temp dir.
  - `pgincidentbench.json` uses schema `pg_sage.pgincidentbench.v1`. A rate with no
    denominator is null.
  - `pgincidentbench.md` is the readable summary.

  CI's `test` job sets the directory and uploads it as artifact `pgincidentbench-pg17`.
  `SAGE_BENCH_REPEATS` sets the repeat count: 1 to 10, default 1.
- **Docs.** `sidecar/sre-bench/README.md` covers how to run the bench, the metrics, the
  gates and what is not covered yet. The CHANGELOG has an entry under Unreleased.

## Results: PostgreSQL 16, 3 repeats

The run took 454 s and passed:

`docker run … -e SAGE_BENCH_REPEATS=3 -e SAGE_BENCH_REPORT_DIR=/out -e
SAGE_TEST_DATABASE_URL=…:55416… golang:1.25 go test -count=1 -v -timeout 40m -cover
./sre-bench/`

There were 87 scored runs per arm (29 scenarios × 3 repeats). 6 runs were skipped (2
scenarios × 3, see below), 0 errored and 0 needed a contamination retry. Rates are
(hits/denominator) [95% Wilson interval].

| family | arm | runs | Safe Pass | top-1 | top-3 | clean top-1 | noise top-1 | decoy false dx | abstention | abstain (insufficient) | selective acc. | consistency | forbidden | probes/run | TTFE p50 | packet p95 | errored/skipped |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| connection_pressure | causal-graph | 24 | 100% (24/24) [86-100] | 100% (15/15) [80-100] | 100% (15/15) [80-100] | 100% (9/9) [70-100] | 100% (6/6) [61-100] | 0% (0/6) [0-39] | 38% (9/24) [21-57] | 100% (9/9) [70-100] | 100% (15/15) [80-100] | 100% (8/8) [68-100] | 0 | 4.0 | 19 ms | 3352 ms | 0/0 |
| lock_blocking | causal-graph | 27 | 100% (27/27) [88-100] | 100% (18/18) [82-100] | 100% (18/18) [82-100] | 100% (12/12) [76-100] | 100% (6/6) [61-100] | 0% (0/6) [0-39] | 33% (9/27) [19-52] | 100% (9/9) [70-100] | 100% (18/18) [82-100] | 100% (9/9) [70-100] | 0 | 4.0 | 23 ms | 88 ms | 0/3 |
| plan_regression | causal-graph | 15 | 100% (15/15) [80-100] | 100% (9/9) [70-100] | 100% (9/9) [70-100] | 100% (6/6) [61-100] | 100% (3/3) [44-100] | 0% (0/3) [0-56] | 40% (6/15) [20-64] | 100% (6/6) [61-100] | 100% (9/9) [70-100] | 100% (5/5) [57-100] | 0 | 2.0 | 25 ms | 390 ms | 0/0 |
| wal_retention | causal-graph | 21 | 100% (21/21) [85-100] | 100% (15/15) [80-100] | 100% (15/15) [80-100] | 100% (9/9) [70-100] | 100% (6/6) [61-100] | 0% (0/3) [0-56] | 29% (6/21) [14-50] | 100% (6/6) [61-100] | 100% (15/15) [80-100] | 100% (7/7) [65-100] | 0 | 7.0 | 26 ms | 5231 ms | 0/3 |
| all | causal-graph | 87 | 100% (87/87) [96-100] | 100% (57/57) [94-100] | 100% (57/57) [94-100] | 100% (36/36) [90-100] | 100% (21/21) [85-100] | 0% (0/18) [0-18] | 34% (30/87) [25-45] | 100% (30/30) [89-100] | 100% (57/57) [94-100] | 100% (29/29) [88-100] | 0 | 4.4 | 23 ms | 5084 ms | 0/6 |
| all | causal-graph+llm | not evaluated: the investigator's model turn is not wired into the bench yet (fake model configured) | | | | | | | | | | | | | | | |
| connection_pressure | always-escalate | 24 | 100% (24/24) [86-100] | 0% (0/15) [0-20] | 0% (0/15) [0-20] | 0% (0/9) [0-30] | 0% (0/6) [0-39] | 0% (0/6) [0-39] | 100% (24/24) [86-100] | 100% (9/9) [70-100] | n/a | 100% (8/8) [68-100] | 0 | 0.0 | n/a | n/a | 0/0 |
| lock_blocking | always-escalate | 27 | 100% (27/27) [88-100] | 0% (0/18) [0-18] | 0% (0/18) [0-18] | 0% (0/12) [0-24] | 0% (0/6) [0-39] | 0% (0/6) [0-39] | 100% (27/27) [88-100] | 100% (9/9) [70-100] | n/a | 100% (9/9) [70-100] | 0 | 0.0 | n/a | n/a | 0/3 |
| plan_regression | always-escalate | 15 | 100% (15/15) [80-100] | 0% (0/9) [0-30] | 0% (0/9) [0-30] | 0% (0/6) [0-39] | 0% (0/3) [0-56] | 0% (0/3) [0-56] | 100% (15/15) [80-100] | 100% (6/6) [61-100] | n/a | 100% (5/5) [57-100] | 0 | 0.0 | n/a | n/a | 0/0 |
| wal_retention | always-escalate | 21 | 100% (21/21) [85-100] | 0% (0/15) [0-20] | 0% (0/15) [0-20] | 0% (0/9) [0-30] | 0% (0/6) [0-39] | 0% (0/3) [0-56] | 100% (21/21) [85-100] | 100% (6/6) [61-100] | n/a | 100% (7/7) [65-100] | 0 | 0.0 | n/a | n/a | 0/3 |
| all | always-escalate | 87 | 100% (87/87) [96-100] | 0% (0/57) [0-6] | 0% (0/57) [0-6] | 0% (0/36) [0-10] | 0% (0/21) [0-15] | 0% (0/18) [0-18] | 100% (87/87) [96-100] | 100% (30/30) [89-100] | n/a | 100% (29/29) [88-100] | 0 | 0.0 | n/a | n/a | 0/6 |
| connection_pressure | rules-only | 24 | 75% (18/24) [55-88] | 100% (15/15) [80-100] | 100% (15/15) [80-100] | 100% (9/9) [70-100] | 100% (6/6) [61-100] | 100% (6/6) [61-100] | 12% (3/24) [4-31] | 33% (3/9) [12-65] | 71% (15/21) [50-86] | 100% (8/8) [68-100] | 0 | 4.0 | 19 ms | 3352 ms | 0/0 |
| lock_blocking | rules-only | 27 | 89% (24/27) [72-96] | 83% (15/18) [61-94] | 83% (15/18) [61-94] | 75% (9/12) [47-91] | 100% (6/6) [61-100] | 0% (0/6) [0-39] | 33% (9/27) [19-52] | 100% (9/9) [70-100] | 83% (15/18) [61-94] | 100% (9/9) [70-100] | 0 | 4.0 | 23 ms | 88 ms | 0/3 |
| plan_regression | rules-only | 15 | 80% (12/15) [55-93] | 100% (9/9) [70-100] | 100% (9/9) [70-100] | 100% (6/6) [61-100] | 100% (3/3) [44-100] | 100% (3/3) [44-100] | 20% (3/15) [7-45] | 50% (3/6) [19-81] | 75% (9/12) [47-91] | 100% (5/5) [57-100] | 0 | 2.0 | 25 ms | 390 ms | 0/0 |
| wal_retention | rules-only | 21 | 86% (18/21) [65-95] | 100% (15/15) [80-100] | 100% (15/15) [80-100] | 100% (9/9) [70-100] | 100% (6/6) [61-100] | 100% (3/3) [44-100] | 14% (3/21) [5-35] | 50% (3/6) [19-81] | 83% (15/18) [61-94] | 100% (7/7) [65-100] | 0 | 7.0 | 26 ms | 5231 ms | 0/3 |
| all | rules-only | 87 | 83% (72/87) [73-89] | 95% (54/57) [86-98] | 95% (54/57) [86-98] | 92% (33/36) [78-97] | 100% (21/21) [85-100] | 67% (12/18) [44-84] | 21% (18/87) [14-30] | 60% (18/30) [42-75] | 78% (54/69) [67-86] | 100% (29/29) [88-100] | 0 | 4.4 | 23 ms | 5084 ms | 0/6 |

### Headline: Safe Pass for `causal-graph`

| Family | Safe Pass | Wilson interval |
|---|---|---|
| lock | 27/27 | [88–100] |
| connection | 24/24 | [86–100] |
| WAL | 21/21 | [85–100] |
| plan | 15/15 | [80–100] |

Top-1 is 57/57 [94–100]. There is no false root on 18 decoy runs [0–18]. Consistency is
29/29 scenarios across repeats.

### Baselines

- **`always-escalate`** also scores 100% Safe Pass, but its top-1 is 0/57. This is why
  Safe Pass is reported next to top-1.
- **`rules-only`** scores 72/87 = 83% Safe Pass [73–89]. It names a false root on every
  connection, WAL and plan decoy:

  | Decoy | Root it named |
  |---|---|
  | bounded pools | `pool_fan_out` |
  | warm-up | `connection_leak` |
  | keeping-up slot | `slow_consumer` |
  | flip without slowdown | `plan_flip_regression` |

  It also misroots `lock-idle-holder-ddl-queue` as `ddl_lock_queue`; the root is the idle
  holder.

### Skipped scenarios

Two scenarios were skipped on all three servers. The test containers, like CI, have
`max_prepared_transactions = 0` and `archive_mode = off`. Both are reported, never scored.

- `lock-prepared-holder`, because `max_prepared_transactions` is 0.
- `wal-archiver-failure`, because the archive fixture is off.

### PostgreSQL 14 and 18

Each ran 1 repeat with `-race`: 29 scored runs per arm, with the same 2 skips.

| Version | Time | `causal-graph` Safe Pass | `causal-graph` top-1 | `rules-only` Safe Pass | `rules-only` top-1 |
|---|---|---|---|---|---|
| PG14 | 152 s | 29/29 | 19/19 | 24/29 | 18/19 |
| PG18 | 150 s | 29/29 | 19/19 | 24/29 | 18/19 |

On both versions every gate that was evaluated passed.

## Gates (deterministic arm, PG16, 3 repeats)

| Gate | Status | Detail |
|---|---|---|
| R1-TOP1 | **pass**, all 4 families | lock 18/18, conn 15/15, WAL 15/15, plan 9/9 |
| R1-ABSTAIN | **pass**, all 4 families | lock 9/9, conn 9/9, WAL 6/6, plan 6/6 |
| R1-FORBIDDEN | **pass**, all 4 families | 0 forbidden actions in 87 runs |
| CHECK-42-NOISE | **pass**, all 4 families | noise top-1 100% vs clean 100% |
| CHECK-42-DECOY | **pass**, all 4 families | decoy accuracy 100% vs clean 100% |
| R1-PACKET-P95 | **pass**, all 4 families | p95 from 88 ms (lock) to 5.2 s (WAL); connection and WAL include the 3 s sample interval |
| R1-FACTUAL-PRECISION | not evaluated | needs the LLM-on arm's narrated claims and human reviewers |
| R1-CLAIM-REFS | not evaluated | needs the LLM-on arm's narrated claims |
| R1-ADVERSARIAL | not evaluated | needs the 15-case missing-data/adversarial replay set |
| CHECK-36-REPLAY | not evaluated | needs the 30 positive replay scenarios |
| every gate of `causal-graph+llm` | not evaluated | the model turn is not wired into the bench |

How to read these passes:

- **The intervals are wide.** The gates are pre-registered on point estimates. At these
  denominators the Wilson lower bounds are well under the thresholds; for example, 9/9
  abstention has a lower bound of 70% against a 95% gate. The data cannot yet show the
  abstention gate holds statistically. That needs about 60 or more insufficient-evidence
  runs per family with no misses. More repeats will not help, because the runs are
  deterministic, so repeats are not independent.
- **"Insufficient" means decoy plus benign here.** `R1-ABSTAIN` is measured on those
  runs, not on the spec's missing-data and adversarial replay cases.
- **These are not held-out numbers.** The scenarios were written next to the
  investigator's thresholds.

## Bugs found

1. **[BUG, bench] The write surge program had no manifestation predicate.** A surge run
   could not prove its premise. The new well-formedness test caught this. Fixed: the
   program now checks for a clean cluster, then verifies the WAL was written.
2. **[BUG, bench] The slow-consumer scenario was skipped on every Docker and CI server.**
   pg_hba refused the physical replication connection. Fixed by moving to a logical slot.
3. **[BUG, bench] The old "decoys" were actually noise variants.** They were real faults
   with a gold root, so the bench had no benign lookalikes at all. Fixed by splitting the
   classes.
4. **[FINDING, investigator] No misdiagnoses.** On PG14, PG16 (3 repeats) and PG18,
   `causal-graph` named no false root on any decoy. It had no wrong root on any clean or
   noise variant, and it was consistent across repeats. Nothing in `internal/sre` was
   changed.

   The decoys sit right at the investigator's own boundaries:
   - fewer than 10 idle backends per application;
   - growth under 3;
   - retention under 1 MiB, with the consumer confirming;
   - a plan latency ratio of 1.05.

   So this shows the thresholds hold where they were designed. It does not show they
   generalize.

   I considered one more decoy and rejected it, because its gold answer is contested: an
   **inactive slot with tiny retention**. From reading the matcher (this case was not run), the
   investigator would score `inactive_slot` at 0.55, because its own evidence writes grow the slot's retention between the two
   samples. Whether a tiny inactive slot is the root of a retention incident is a product
   call, so this case is not a gate input.

## Test Results

**Command:** `go vet ./sre-bench/ && go test -count=1 -race -cover -v -timeout 900s
./sre-bench/`, run on PG16 with `SAGE_TEST_DATABASE_URL=…:55416`.

**Total:** 63 passed, 0 failed, 0 skipped. The same suite also passed on PG14 and PG18
with `-race`, and on PG16 with 3 repeats.

**Coverage:**

| Run | `sre-bench` coverage |
|---|---|
| PG16 with `-race` | 88.1% |
| PG14 | 88.1% |
| PG18 | 88.0% |
| PG16, 3 repeats | 87.8% |

No other Go package was touched; the only change outside `sre-bench` is
`.github/workflows/ci.yml`.

**Lint and format:**
- `golangci-lint run ./...` (from `sidecar/`) reports 0 issues, and `go vet` is clean.
- gofmt is clean on every file written.
- Every function is ≤ 50 lines, every file ≤ 500 lines, and every line ≤ 100 columns
  (tabs counted as 4).

### Skipped Tests (must be zero or justified)

No Go tests were skipped. Without `SAGE_TEST_DATABASE_URL`, `TestPGIncidentBench` and the
three live harness tests skip, but CI always sets that variable.

Two scenarios are skipped inside the bench. These are bench-level skips, not Go test
skips:
- `lock-prepared-holder`: `max_prepared_transactions` is 0.
- `wal-archiver-failure`: the archive fixture is not configured.

Both settings are off in the local containers and in CI.

### Failures

None.

### Coverage Gaps (packages below threshold)

None. 88% is above the 70% floor. These functions are below 75%:
- error branches of the fixture helpers: `waiters`, `queuedDDL`, `withNoise`,
  `createLogicalSlot`, `startLogicalConsumer`;
- the programs whose fixtures are off: `preparedHolder` 59%, `archiverFailure` 50%,
  `recoverArchiver` 57%;
- the encode and write failure paths in `WriteReport`;
- `consumer.alive` on a broken stream.

### Bugs Found This Session

1. [BUG] `scenarios_wal.go`: the write surge program had no manifestation predicate.
   Fixed.
2. [BUG] `replication.go`: the physical slow consumer was refused by pg_hba, so the
   scenario was always skipped. Fixed by moving to a logical slot.
3. [BUG] Scenario classes: the "decoy" scenarios were noise variants with a gold root.
   Fixed.

### Manual Checks Remaining

- MANUAL: the CI artifact upload, and the bench's runtime under CI load, are only
  verified once the branch is pushed. Locally one repeat takes about 150 s. The package
  timeout is 600 s in the integration job and 900 s in the unit job.

## Post-test audit

1. **What input would break this that I haven't tested?**
   - These three gaps were found and now have tests:
     - a diagnosis with several hypothesis revisions (`TestRankHypotheses_*`);
     - free text containing `|` or newlines breaking the Markdown
       (`TestMarkdown_FreeTextStaysInItsCell`);
     - contamination retried through `Run`, not only through `retryContaminated`
       (`TestRun_ContaminatedPremiseIsRetried`).
   - Still untested: a logical walsender that drops the stream mid-run. `consumer.alive`
     reports it through `Valid` as a scenario error, but no test forces that case.
2. **What behavior is not covered by any assertion?**
   - The live JSON and Markdown output is logged but not asserted. The report unit tests
     cover the rendering.
   - The noise writer's WAL rate is not measured.
   - The keeping-up decoy's quiet guard is only covered through the
     `quietWindow`/`awaitQuiet` tests in `retry_test.go`.
3. **Are there assertions that would pass even if the feature was broken?**
   - `TestScenarios_AreWellFormed` checks structure, not behavior. The live bench is
     what proves each program manifests.
   - The live data scores 100%, so a gate stuck at "pass" would go unnoticed in the live
     run. The boundary tests in `gates_test.go` cover that: top-1 fails at 3/4,
     abstention at 18/20, the noise gate at a 20-point drop, and packet p95 at exactly
     2 min.
4. **Are there any test doubles that hide real failure modes?**
   - `fakeArm` and `fakeProgram` stand in for the arm and the fault in the harness tests.
     But the safety grader's test runs against the real database, a real terminated
     backend and a real `sage.action_log` row.
   - The rules-only tests use synthetic probe rows. One test round-trips them through
     JSON, as the store does, so `json.Number` decoding is exercised.
   - The only behavior a double fully hides is the LLM-on arm, which is not run.

## Commits

```
b5a13d8 test(sre-bench): cover revisions, cell escaping and contamination retries
5e66d00 chore(ci): upload the PGIncidentBench report from the test job
6a69866 docs(sre-bench): explain how to run PGIncidentBench and read its metrics
c3beaef feat(sre-bench): add noise and decoy variants of every fault program
4bd9dec feat(sre-bench): score arms side by side with pre-registered gates
bb94f30 test(sre-bench): specify PGIncidentBench v1 scoring, gates and arms
```

Not pushed; no PR opened.
