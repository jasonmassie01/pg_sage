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
| `SAGE_BENCH_REPEATS` | `1` | How many times each scenario runs (1 to 10). Run-to-run consistency needs at least 2. One repeat of every family takes about 15 minutes; run `go test -timeout 40m` per repeat. |
| `SAGE_BENCH_FAMILIES` | all | Comma-separated families to run, for example `checkpoint_storm,lwlock_contention`. An unknown family fails the test. |
| `SAGE_BENCH_REPORT_DIR` | the test's temp dir | Where `pgincidentbench.json` and `pgincidentbench.md` are written. CI sets this and uploads the directory. |
| `PG_SAGE_BENCH_LLM_URL` | unset | OpenAI-compatible endpoint for the LLM-on arm (opt-in). If unset, the LLM-on arm uses the deterministic fake model. |
| `PG_SAGE_BENCH_LLM_MODEL` | unset | Model name. Required when the URL is set. |
| `PG_SAGE_BENCH_LLM_KEY` | unset | API key. Optional, because local models need none. The key is never written to the report. |

Some fixtures depend on the server. The prepared-transaction scenario needs
`max_prepared_transactions > 0`. The archiver scenario needs
`archive_mode = on` and `archive_command = 'test ! -s /tmp/pg_sage_archive_fail'`.
The logical-slot and replication lag scenarios need `wal_level = logical`. The
checkpoint scenarios need permission for `CHECKPOINT` and `ALTER SYSTEM`; the
undersized program sets `max_wal_size = '32MB'` and always resets it, and it
refuses to run where `max_wal_size` is already set by `ALTER SYSTEM`. The
burst decoy needs `max_wal_size` of at least 512 MiB. The temp-file
scenarios read live temp files with `pg_ls_tmpdir` (superuser or
`pg_monitor`), and the repeated-spill and workload scenarios need
`pg_stat_statements` preloaded. If the server lacks a fixture, that scenario
is reported as skipped. A skipped scenario is never scored.

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
| `decoy` | A benign lookalike of a fault. Examples: a lock wait that resolves, an idle transaction that blocks nobody, small pools that add up, a pool warming up, a logical slot whose consumer keeps up, a plan flip with no slowdown, a WAL burst that fits within `max_wal_size`, a temp spill that already finished, a small live spill, a replica that keeps up with a burst, a few light commits. | Inconclusive. Any root is a false diagnosis. |
| `benign` | No fault. | Inconclusive. |

The bench covers the R1 families (lock blocking, connection pressure, WAL and
replication retention), plan regression and the M6 reactive families:

| Family | Clean faults | Noise | Decoys / benign |
|---|---|---|---|
| `checkpoint_storm` | `max_wal_size` at 32MB under 64 MiB bursts; a script issuing `CHECKPOINT` twice a second | the undersized fault under load | a 96 MiB burst inside `max_wal_size`; a quiet cluster |
| `temp_file_explosion` | a query holding a 100+ MiB live spill; one statement spilling about 25 MB per call; four statements each spilling a few MB | the runaway query under load | a large spill that finished before the run; a 16 MB live spill; no temp files |
| `replication_lag` | a logical replica confirming writes but not replay; one confirming writes but not flushes | the replay backlog under load | a replica that keeps up with a 48 MiB burst; no replicas |
| `lwlock_contention` | 24 sessions committing one-row inserts (WAL write); 32 sessions querying a 100-partition table without pruning (lock manager) | the commit storm under load | two light writers; an idle cluster |

The replicas are logical replication consumers on the bench's own server,
so standby-side mechanisms (paused replay, standby queries holding replay
back) and "WAL not yet sent" are covered by the matchers' unit tests, not by
fault programs. A consumer that stops reading does not back-pressure the
walsender behind Docker's port proxy, which buffers what it sends. LWLock
load runs in server-side loops: with a client round trip per statement most
sessions sit idle and contention comes and goes. LWLock decoy and benign
runs watch other databases' LWLock waits and are repeated when that load was
there.

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
