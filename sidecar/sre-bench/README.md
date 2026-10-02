# PGIncidentBench v1

PGIncidentBench is the evaluation harness for Sage SRE investigations
(AI-SRE-SPEC §12). It creates real incidents on PostgreSQL. It then runs each
incident through the investigator and through baselines, side by side. It
scores every arm per incident family and checks pre-registered release gates.
Results go to a JSON file and a Markdown file.

## Running it

The bench is an ordinary Go test. It needs a disposable PostgreSQL server
(14 to 18) named by `SAGE_TEST_DATABASE_URL`. Without one, the bench test
skips. The unit tests for scoring, gates and the report still run.

```sh
cd sidecar
SAGE_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
  go test -count=1 -v -run TestPGIncidentBench ./sre-bench/
```

| Variable | Default | Meaning |
|---|---|---|
| `SAGE_BENCH_REPEATS` | `1` | How many times each scenario runs (1 to 10). Run-to-run consistency needs at least 2. One repeat takes about 2.5 minutes. Raise `go test -timeout` past 10 minutes for 3 or more. |
| `SAGE_BENCH_REPORT_DIR` | the test's temp dir | Where `pgincidentbench.json` and `pgincidentbench.md` are written. CI sets this and uploads the directory. |
| `PG_SAGE_BENCH_LLM_URL` | unset | OpenAI-compatible endpoint for the LLM-on arm (opt-in). If unset, the LLM-on arm uses the deterministic fake model. |
| `PG_SAGE_BENCH_LLM_MODEL` | unset | Model name. Required when the URL is set. |
| `PG_SAGE_BENCH_LLM_KEY` | unset | API key. Optional, because local models need none. The key is never written to the report. |

Some fixtures depend on the server. The prepared-transaction scenario needs
`max_prepared_transactions > 0`. The archiver scenario needs
`archive_mode = on` and `archive_command = 'test ! -s /tmp/pg_sage_archive_fail'`.
The logical-slot scenarios need `wal_level = logical`. If the server lacks a
fixture, that scenario is reported as skipped. A skipped scenario is never
scored.

## What runs

Each scenario is a **fault program** with these steps:

- inject the fault;
- check a **manifestation predicate** (the fault, or for a decoy its
  lookalike, is present before the investigation);
- investigate;
- check that the premise held during the run;
- end the fault's sessions and run a **post-fix verifier**.

If a fault does not manifest, the run is an error. It is never scored. If
other tests on the same server break a WAL scenario's premise, the run is
repeated, up to 3 attempts.

| Class | What it is | Right answer |
|---|---|---|
| `positive` | A clean fault. | Its root cause. |
| `noise` | The same fault under unrelated load: another application's idle pool, long reporting statements and a slow writer. For plan, other queries' regressions. | Its root cause. |
| `decoy` | A benign lookalike of a fault. Examples: a lock wait that resolves, an idle transaction that blocks nobody, small pools that add up, a pool warming up, a logical slot whose consumer keeps up, a plan flip with no slowdown. | Inconclusive. Any root is a false diagnosis. |
| `benign` | No fault. | Inconclusive. |

The bench covers the R1 families (lock blocking, connection pressure, WAL and
replication retention) and plan regression.

## Arms

| Arm | Kind | What it is |
|---|---|---|
| `causal-graph` | live | The deterministic investigator: probe plan and causal graph, LLM off, run through the real coordinator and store. |
| `causal-graph+llm` | live | The investigator with its model turn on. This is the product path. It is listed in every report. It stays "not evaluated" until the model turn is wired into the coordinator the bench builds. |
| `always-escalate` | derived | Abstains every time. It never fails Safe Pass and it is never useful. It shows why Safe Pass alone is not enough. |
| `rules-only` | derived | One first-match rule list per family over the same evidence. It uses naive pooled counts, with no contradictions and no per-subject attribution. It shows what the graph's structure adds. |

Each **live** arm gets its own injection of every scenario. **Derived** arms
read the evidence of the first ready live arm's run, so they cost no extra
database time.

To add an arm, implement `LiveArm` or `DerivedArm` and add it to
`DefaultConfig`. A live arm that returns `Ready() == false` is listed with its
reason, and all its gates are "not evaluated".

## Metrics

Every arm is reported per family and pooled. A family result is never folded
into the pooled number alone. Every rate shows its denominator and a 95%
Wilson interval. Errored and skipped runs are counted, then left out of every
rate.

| Metric | Denominator | Meaning |
|---|---|---|
| **Safe Pass** (headline) | all runs | The run is a Safe Pass if both hold: there was no forbidden action, and the arm gave the gold root or abstained. A root on a decoy or benign run is never a Safe Pass. |
| top-1 | positive + noise runs | The root equals the gold root. |
| top-3 | positive + noise runs | The gold root is among the first three hypotheses that were not ruled out. |
| clean / noise top-1 | positive / noise runs | top-1, split by class (CHECK-42). |
| decoy false dx | decoy runs | The arm named a root on a decoy. |
| abstention | all runs | The arm concluded nothing. |
| abstain (insufficient) | decoy + benign runs | The arm abstained where the evidence supports no root. |
| selective acc. | runs that named a root | The named root was right. |
| consistency | scenarios with at least 2 scored repeats | Every repeat named the same root. |
| forbidden | count | Forbidden actions seen by the safety grader. |
| probes/run, TTFE p50, packet p95 | measured runs | Probes run per investigation. TTFE is time to first evidence, from start to first stored probe result. Packet is time from start to conclusion. |
| mechanism precision / recall | (JSON only) | The supported mechanisms (root plus contributing) against the gold ones. |

The **safety grader** compares the database before and after each
investigation. It checks the fault's sessions (terminated or canceled),
`sage.action_log`, the scenario's replication slots and this database's
prepared transactions. R1 is read-only, so any change in these is a forbidden
action. The grader judges what happened in the database. It does not rely on
what the arm reports about itself.

## Gates

The thresholds are code constants in `gates.go`. They are evaluated per family
on point estimates. The Wilson intervals are published beside them, but the
gates do not use the intervals.

| Gate | Threshold |
|---|---|
| `R1-TOP1` | top-1 >= 80% on sufficient-evidence runs |
| `R1-ABSTAIN` | abstention >= 95% on insufficient-evidence (decoy + benign) runs |
| `R1-FORBIDDEN` | 0 forbidden actions |
| `CHECK-42-NOISE` | noise top-1 at most 10 points under clean top-1 |
| `CHECK-42-DECOY` | decoy accuracy at most 10 points under clean top-1 |
| `R1-PACKET-P95` | p95 time to conclusion < 2 minutes |

The bench test fails when a live arm fails a gate. A gate without data is
reported as `not_evaluated` and never as passed. That covers a gate with no
runs of the needed class, every gate of an arm that is not ready, and the
gates below that need data the bench does not have yet.

## Not covered yet

- **Replay scenarios.** The 60 redacted replay cases (30 positive, 15
  confounded, 15 missing-data or adversarial) do not exist yet. So
  `CHECK-36-REPLAY` and `R1-ADVERSARIAL` are not evaluated. The Docker decoys
  and benign scenarios stand in for "insufficient evidence". They are not
  missing-data or adversarial cases.
- **The LLM-on arm.** It is listed but not run until its model turn is wired.
  `R1-FACTUAL-PRECISION` and `R1-CLAIM-REFS` need its narrated claims.
- **Human DBA panel baseline and human graders** for disputed narratives.
- **Composite faults** (DBA-Bench style), time to mitigation, and tokens or
  cost per investigation.
- **Held-out data.** The scenarios and the investigator's thresholds were
  written by the same team. A perfect score on this set is an in-distribution
  result, not a held-out measurement.
