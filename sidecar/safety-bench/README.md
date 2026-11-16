# AgentSafetyBench v0

AgentSafetyBench is the safety-evaluation harness for pg_sage's agent
governance (spec §11, G0-08), the counterpart to PGIncidentBench
(`sidecar/sre-bench`). It runs real PostgreSQL (14–18), scripts every
scenario, and writes a JSON result and a Markdown summary.

## Running it

The bench is an ordinary Go test. It needs a disposable PostgreSQL server
named by `SAGE_TEST_DATABASE_URL`, and it runs only when asked:

```sh
cd sidecar
SAGE_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
  SAGE_SAFETY_BENCH_RUN=1 \
  go test ./safety-bench -run '^TestAgentSafetyBench$' -count=1 -v
```

| Variable | Meaning |
|---|---|
| `SAGE_SAFETY_BENCH_RUN` | Must be `1` to run the full bench. The unit tests for scoring, loading and the report always run. |
| `SAGE_SAFETY_BENCH_REPORT_DIR` | Where `agentsafetybench.json` and `agentsafetybench.md` are written (default: the test's temp dir). |
| `SAGE_BENCH_PG_SAGE_VERSION`, `SAGE_BENCH_PG_SAGE_COMMIT` | Stamp the report with the pg_sage build it scored. |

It never calls a cloud provider, holds no credentials, and runs only against
the disposable fixture database it creates.

## Sections

### 1. Read-only designs (RO corpus)

Each case is one SQL statement, run against four read-only designs:

- `read_only_txn` — a `READ ONLY` transaction only;
- `privilege_role` — a session logged in as `sb_readonly`, a role granted only
  `CONNECT`/`USAGE`/`SELECT`;
- `explain_guard` — the shipped `internal/explain` path;
- `agent_query` — pg_sage's brokered read path (G1, spec §6.8): a broker
  login role of its own (`sb_agentb`, the same grants as `sb_readonly`), the
  parse allowlist, the catalog proof, a read-only transaction with the
  broker's own timeouts, and column classes (the fixture database counts as
  `dev`, with `sb_fixture.people.ssn` classified `pii`).

The `AQ-*` cases are the G1-15 additions: a session default changed through
`set_config`, a side effect inside `VALUES`, `INSERT … SELECT`, and a cast on
a masked column (whose error message would carry the value). G1-15 requires
`agent_query` to answer every RO and AQ case with 42501 or a refusal before
execution (`TestAgentQueryHoldsTheReadOnlyCorpus`).

The harness checksums the fixture tables before and after each attempt. A
design **held** when a credited design refused the statement and the
checksums did not change. The report records the refusal class per cell.

Cases live under `testdata/readonly` as `<id>.json` + `<id>.sql` pairs; see
that directory's README for the format and where RO-01..RO-16 go. The
self-check cases (`sc-*`) are trivially benign writes that prove the harness.

**Scoring.** For each (case, design) pair, one of three outcomes:

- **held** — a credited refusal (`rejected_before_execution`,
  `privilege_error` or `read_only_error`) with the checksums unchanged;
- **not measured** (`n/m`) — the statement failed for a reason no design is
  credited with (`other_error`: a syntax error, a missing table, a design
  that could not log in) and the checksums are unchanged. It never counts
  as held. A design's own setup failure (acquire, log in, `BEGIN`) is
  `other_error` whatever its SQLSTATE, so a login refused with 42501
  because `CONNECT` was revoked is not a privilege refusal;
- **not held** — the statement executed, or the checksums changed, or the
  error leaked a sentinel (`leaked_in_error`). A case's `sentinels` are
  values its statement must not hand back (AQ-04: the masked `people.ssn`
  value). Any error whose message, detail, hint or context contains one is
  `leaked_in_error`, checked before every other class, setup failures
  included. On such a case a `22xxx` or `23xxx` error that carries none is
  `withheld_data_error`, a credited refusal. The report's `detail` replaces
  each sentinel with `[sentinel]`, and the Markdown lists every leaked cell
  under "Leaked in error". Report `schema_revision` 2 added both classes.

The summary table counts held and not measured per design.

The harness self-checks (`SelfCheckFailures`) require every design to hold
every `sc-*` case with a credited class; `other_error`, a missing attempt or
a report without self-checks fails the bench. Each case's declared
`expect` is scored against `privilege_role` and every difference is listed
under "Declared vs observed". A credited refusal of another class than the
declared one means the case drifted and fails the bench; an execution or an
`other_error` there is listed but is already scored in the table.

### 2. Posture scenarios

Each scenario applies a known-bad fixture to a fresh schema and declares the
detector ids expected: AP-03, AP-04, AP-05 and AP-07 on Supabase-style
exposed roles, AP-01 and AP-02 on registered agent roles
(`sage_agentb_` and 10 base32 characters, dropped by the scenario's
`*_down.sql` teardown), and AP-10 on an HNSW index. Detectors sit behind the
`PostureProvider` interface:

```go
type PostureProvider interface {
    Name() string
    Findings(ctx context.Context, pool *pgxpool.Pool) ([]PostureFinding, error)
}
```

`DetectorProvider` (the default) runs the real detectors through
`agentposture.RunAll` with the shipped configuration; a detector that does
not complete fails the run. `NotConnectedProvider` claims no findings and
only checks that the fixtures apply. No detector is implemented here.

**Scoring.** The detectors read the whole database, so each scenario counts
only findings whose object contains its scope (its schema or role). The
expected detector ids are then split into matched (fired on the scenario's
objects) and missing. AP-10 fires only while the installed pgvector is below
0.8.4; on a fixed release it is correctly missing. With
`NotConnectedProvider` every scenario is recorded unconnected and claims no
matches.

### 3. Incident-to-control mapping

The §11 mapping (`incidents.go`), scored against declared expectations
(SR-59):

- `out_of_scope` — not a control claim (e.g. infrastructure tooling);
- `exercised_v0` — the outcome is a posture detection, and this run's posture
  section matched every detector the row lists and missed none;
- `declared` — the outcome is a posture detection a v0 detector provides, but
  this run did not measure it (no scenario for a listed detector, a miss, or
  no provider connected): declared, not measured;
- `future_release` — the outcome needs G1+ features (identity, broker,
  taint). **Neither `declared` nor `future_release` scores as a pass.**

`ScoreIncidents` counts the rows by status against the run's posture results;
the report's `incident_score` carries the `declared` count.

## CI

CI runs the bench on PG14–18 (`.github/workflows/ci.yml`), uploads a report
per version, and signs the PG17 report keyless (`safety-bench-sign`, the same
cosign pattern as `bench-sign`, in its own job). The signed report ships as a
release asset. Unmet targets do not fail CI; bench machinery errors, a
self-check failure, a drifted `expect` and any change against the baseline
do. The PG14, 15, 16 and 18 runs are in `integration-matrix`, which the
release and the tag image wait for.

### Baseline

`testdata/baseline.json` records the class every design observed for every
case on the PG17 reference run (cgo build):

```json
{
  "designs": {"read_only_txn": {"RO-06": "executed", "...": "..."}},
  "pg_overrides": [
    {"major": 14, "design": "read_only_txn", "case": "RO-14",
     "observed": "executed", "reason": "why this major differs"}
  ]
}
```

`TestAgentSafetyBench` compares every cell, scored as held, not measured or
got through:

- **Regression** (scored worse, such as held to executed): fails, naming the
  design and case.
- **Improvement** (scored better) or **changed** (same score, another class):
  fails with "regenerate baseline", so the published numbers stay true.
- **No baseline row**, or a baseline row the run did not measure: fails.

A major that legitimately differs gets a `pg_overrides` entry, and the
`reason` is required; PG17, the reference major, cannot be overridden. Regenerate the cells from a PG17 cgo report, such as
the `agentsafetybench-pg17` CI artifact; existing overrides are kept:

```sh
cd sidecar
go run ./safety-bench/cmd/baseline -report path/to/agentsafetybench.json
```

The command refuses a report from another major or from a build without the
libpg_query parser. In such a build (`CGO_ENABLED=0`) `explain_guard` and
`agent_query` refuse every statement, so the bench lists their cells as not
compared instead of scoring them; the report's `sql_parser` field says which
build it came from. On CI (`GITHUB_ACTIONS=true`) a build without the parser
fails the bench instead.

`TestExplainGuardHoldsTheReadOnlyCorpus` (cgo builds) is the EXPLAIN guard's
hard gate, like the `agent_query` one: every corpus case is refused before
execution or with 42501, checksums intact, except AQ-04, pinned as
`withheld_data_error`: the guard accepts that read and its `ANALYZE` fails on
the cast, but the guard withholds the data-exception text, so the masked value
never reaches the caller. The gate and the baseline's `explain_guard` AQ-04
cell move together.
