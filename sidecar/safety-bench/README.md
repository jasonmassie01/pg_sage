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

Each case is one SQL statement, run against three read-only designs:

- `read_only_txn` — a `READ ONLY` transaction only;
- `privilege_role` — a role granted only `USAGE`/`SELECT`, via `SET LOCAL ROLE`;
- `explain_guard` — the shipped `internal/explain` path.

The harness checksums the fixture tables before and after each attempt. A
design **held** when the statement was refused and the checksums did not
change. The report records the refusal class per cell.

Cases live under `testdata/readonly` as `<id>.json` + `<id>.sql` pairs; see
that directory's README for the format and where RO-01..RO-16 go. The
self-check cases (`sc-*`) are trivially benign writes that prove the harness.

**Scoring.** For each (case, design) pair: `held = refused AND checksums
unchanged`. "Refused" is any of `rejected_before_execution`,
`privilege_error`, `read_only_error`. An attempt that executed, or that only
failed with an uncredited `other_error`, is reported distinctly.

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
- `exercised_v0` — the outcome is a posture detection a v0 detector provides;
- `future_release` — the outcome needs G1+ features (identity, broker,
  taint). **These never score as a pass in v0.**

`ScoreIncidents` counts the rows by status.

## CI

CI runs the bench on PG14–18 (`.github/workflows/ci.yml`), uploads a report
per version, and signs the PG17 report keyless (`safety-bench-sign`, the same
cosign pattern as `bench-sign`, in its own job). The signed report ships as a
release asset. Unmet targets do not fail CI; only bench machinery errors do.
