<!-- DRAFT. Not for publication; publishing is the owner's call. -->
<!-- Product name appears once (see "The name" below) so a rename is a
     one-line change. Everywhere else this is "the governed configuration"
     or "pg_sage (governed)". -->

# AgentSafetyBench

AgentSafetyBench is an open, reproducible benchmark that measures how
PostgreSQL stands up to the ways AI agents get databases into trouble. It
runs real PostgreSQL (14 through 18), scripts every scenario, and publishes a
signed report with each release. It is the safety counterpart to
PGIncidentBench, which measures pg_sage's incident investigations.

v0 answers three questions with evidence, not assertions:

1. **Which "read-only" designs actually hold?** Agent tooling keeps shipping
   read-only modes that a crafted statement walks straight through. v0 runs a
   bypass corpus against three designs and reports which design refused which
   case, on real PostgreSQL, with the table contents checksummed before and
   after.
2. **Does posture detection catch the known-bad setups?** A Supabase-style
   exposed table, a permissive policy, a `SECURITY DEFINER` function, `PUBLIC
   CREATE`, an HNSW index on a vulnerable pgvector, an agent role that
   bypasses RLS and an agent role that owns a table each get a fixture; the
   real posture detectors are scored against them.
3. **What would each real-world incident do here?** Every incident in the
   research catalog is mapped to a control and scored against a declared
   expectation: prevented, detected, or out of scope.

Safety is earned, not claimed: v0 scores only what it can measure today, and
records everything else as a future release rather than a pass.

## Method

The bench is an ordinary Go test (`sidecar/safety-bench`), gated by
`SAGE_SAFETY_BENCH_RUN=1` so it runs in its own CI step. It creates a
disposable fixture database, runs the three sections, and writes a JSON
result and a Markdown summary. It never calls a cloud provider, holds no
credentials, and touches only the fixture database it creates.

### Read-only designs

Each corpus case is a single SQL statement with a declared expected refusal.
The harness runs it against three designs:

- **`read_only_txn`** — a `READ ONLY` transaction and nothing else: the
  session's own role, relying on `transaction_read_only`.
- **`privilege_role`** — a role granted only `USAGE` and `SELECT`, reached
  with `SET LOCAL ROLE`. This is the design the bypass research concludes is
  the one that holds: a write fails on privileges regardless of how the
  statement is dressed up.
- **`explain_guard`** — the shipped `internal/explain` path (the one the MCP
  explain tool uses). It accepts a single read statement and declines
  `EXPLAIN ANALYZE` for anything not proven side-effect free, so a write
  never executes.

Around every attempt the harness checksums the fixture tables (an
order-independent content hash) and records whether they changed. A design
**held** for a case when the statement was refused *and* the checksums were
unchanged. The report shows the refusal class for each cell —
`read_only_error`, `privilege_error`, or `rejected_before_execution` — so a
case that "passes" only because of a syntax error is visible rather than
hidden.

The corpus is RO-01..RO-16 (from the read-only bypass research). Each case
is a metadata file plus a `.sql` file under `testdata/readonly`; the harness
embeds and loads them. The fixture statements are the reference — the report
summarizes each bypass *class* in a line and does not publish recipes.

### Posture scenarios

Each scenario applies a known-bad fixture to a fresh schema and declares the
detector ids that should fire (for example, an exposed table expects AP-03).
The bench runs the shipped detectors (`agentposture.RunAll`, default
configuration) behind a small `PostureProvider` interface. The detectors read
the whole database, so a scenario counts only findings on its own objects
(its schema or role). Agent scenarios use registered agent role names
(`sage_agentb_` and 10 base32 characters) and drop them afterwards. The
pgvector scenario creates an HNSW index; AP-10 fires while the installed
pgvector lacks the 0.8.4 fix, and correctly stays quiet on a fixed release,
because the bench does not downgrade extensions.

### Incident-to-control mapping

Every incident is a row: its expected disposition (prevented, detected,
contained, out of scope), the controls behind it, and its v0 status.
Scoring is deliberately conservative. A row scores **exercised in v0** only
when its outcome is a posture detection a v0 detector provides. A row whose
outcome needs later features — distinct identity, a credential broker, taint
— is **future release**, and never counts as a pass. Out-of-scope rows (for
example, an infrastructure tool running `terraform destroy`, which is not a
PostgreSQL action) are marked as such.

## Results (measured)

From the PG17 reference run. The read-only section currently carries the
harness self-checks (plain `INSERT`, `UPDATE`, `CREATE TABLE`); RO-01..RO-16
land as their fixtures are authored, with no code change.

**Read-only designs — self-checks**

| Design | Held | Total |
|---|---|---|
| `read_only_txn` | 3 | 3 |
| `privilege_role` | 3 | 3 |
| `explain_guard` | 3 | 3 |

Each design refuses every benign write with checksums intact, by its own
mechanism: `read_only_txn` with a read-only error, `privilege_role` with a
privilege error, `explain_guard` by rejecting the statement before it runs. A
plain `SELECT` executes under all three, confirming the designs do not simply
refuse everything.

**Incident-to-control mapping** — 7 rows: 2 exercised in v0 (INC-01 and the
shared-login ORM reset, both posture detections), 2 out of scope (INC-19,
INC-20 for prevention), 3 future release (INC-04, INC-06, INC-15).

**Posture scenarios** — seven fixtures, each matched by its expected detector
on PG14, PG17 and PG18 (pgvector 0.8.2): AP-03 (exposed table), AP-04
(permissive policy), AP-05 (`SECURITY DEFINER` function), AP-07 (`PUBLIC
CREATE`), AP-01 (agent role with `BYPASSRLS`), AP-02 (agent role owns a
table) and AP-10 (HNSW index below pgvector 0.8.4).

## Limits

- **v0 does not yet run RO-01..RO-16.** The harness is proven with
  self-checks; the corpus numbers appear once the fixtures are authored.
- **Posture covers seven of sixteen detectors.** AP-06, AP-08, AP-09 and
  AP-11..AP-16 have their own tests in `internal/agentposture` but no bench
  scenario yet; session-based ones (AP-13, AP-16) need live client sessions.
- **The mapping scores expectations, not live attacks.** The prevention
  claims for identity/broker/taint rows are explicitly future work, so no row
  reads as passing before the feature exists.
- **No live-LLM arm in v0.** The governed configurations here are
  deterministic SQL paths; a live-LLM arm arrives with later scenarios.
- **Targets are proposals.** CI reports the measured numbers and does not
  hard-fail on unmet targets; only bench machinery errors fail the run.

## The name

The governed configuration this bench measures is pg_sage's **Agent Guard**.
The name is under a trademark check and may change; the bench's own name does
not.

## Running it

```sh
cd sidecar
SAGE_TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable' \
  SAGE_SAFETY_BENCH_RUN=1 \
  go test ./safety-bench -run '^TestAgentSafetyBench$' -count=1 -v
```

CI runs it on PG14–18 and signs the PG17 report keyless (Sigstore, GitHub
OIDC), the same way the PGIncidentBench reports are signed. The signed report
ships as a release asset.
