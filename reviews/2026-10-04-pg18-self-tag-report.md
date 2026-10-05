# pg_sage self-identification in pg_stat_statements on PostgreSQL 14-18

Branch `claude/fix-pg18-self-tag` (stacked on `release/v1.10.0`, #112).

## Claim checked

"On PostgreSQL 18, pg_stat_statements drops leading comments from the stored query text, so
pg_sage's `/* pg_sage */` tag no longer marks its own statements there."

**Verdict: the PostgreSQL behaviour is real; pg_sage was already mostly immune.** Since
b65f9b83 (on master) the pool's wire tagger moves the tag after the first keyword, which
survives on every version. Measuring found three remaining gaps, fixed here.

## Evidence

Throwaway `pgvector/pgvector:0.8.2-pg14/pg17/pg18` containers (14.23, 17.10, 18.4),
`shared_preload_libraries=pg_stat_statements`, `pg_stat_statements.track=top`; statements
sent with pgx (simple and extended protocol), each on its own table so no queryid collides.

| Tag placement (as sent) | PG14 | PG17 | PG18 |
|---|---|---|---|
| leading `/* pg_sage */ SELECT ...` (simple and extended/Parse) | kept | kept | **dropped** (`SELECT id FROM t ...`) |
| leading `/* pg_sage */ CREATE TABLE ...` (utility) | kept | kept | **dropped** |
| leading `-- pg_sage` line comment | kept | kept | **dropped** |
| leading before `(`: `/* pg_sage */ (SELECT ...)` | kept | kept | **dropped** |
| after first keyword `SELECT /* pg_sage */ ...`, `CREATE /* pg_sage */ TABLE` | kept | kept | kept |
| after `(`: `( /* pg_sage */ SELECT ...)` | kept | kept | kept |
| mid-statement / trailing | kept | kept | kept |
| 2nd statement of a multi-statement query, leading tag | kept | kept | **dropped** |
| 2nd statement of a multi-statement query, after keyword | kept | kept | kept |
| through `ConfigurePool`, untagged / leading / labelled source | `SELECT /* pg_sage ... */ ...` | same | same |
| through `ConfigurePool`, `(SELECT ...)` (before this fix) | `/* pg_sage */ (SELECT` | same | **untagged** |
| through `ConfigurePool`, 2nd statement of a multi-statement query (before) | **untagged** | **untagged** | **untagged** |

First text wins (same role): an application's statement first, pg_sage's identical
statement second leaves one entry, **untagged**, `calls=2`; the other way round the entry is
tagged and counts the application's call as pg_sage's. Identical on 14/17/18.

Real sidecar, 5 minutes (standalone, advisory, its own role `sage_run`, pgbench app load):

| | entries of `sage_run` | untagged | untagged outside the sage schema |
|---|---|---|---|
| PG17 before | 727 | 249 (schema DDL, multi-statement strings) | 0 |
| PG18 before | 701 | 249 (same) | 0 |
| PG18 after | 701 | 2 (one migration with backslashes in literals: tagger falls back) | 0 |

Workload exclusion was never broken (untagged entries all reference `sage.`, which the
exclusion regex catches); selfcost, which counts tag-bearing entries only, missed them.

## Changes

- `internal/selfmonitor/tagsplit.go` (new): every statement of a simple Query gets its own
  tag. Boundaries are read outside literals, quoted identifiers, dollar quotes, comments and
  parentheses; on any doubt (unterminated quote/comment, backslash in a literal, whose
  meaning depends on `standard_conforming_strings`) only the first statement is tagged, as
  before, so a tag is never placed inside a literal.
- `tagplace.go`: a parenthesized statement is tagged after its `(`; `keywordEnd` reads `$`
  inside identifiers (fuzzing found the tag splitting `A$$`).
- `pool.go`: `ConfigureConn` for single connections; `ConfigurePool` uses it.
- `internal/api/database_helpers.go`: the connection test connects through `ConfigureConn`
  (it ran `SELECT version()` untagged and unnamed on the tested database).
- Matching unchanged: `IsTagged` (anywhere), selfcost `strpos(query, '/* pg_sage') > 0`,
  workload/`StatementExclusionSQL` `ILIKE '%pg_sage%'` are placement-independent.

## Product decisions

- **Text tag, not queryids.** The comment does not change the queryid and pg_stat_statements
  keys entries by (userid, dbid, queryid, toplevel), so a registry of pg_sage's queryids
  cannot separate its calls from an identical application statement either (same entry).
  Learning each own queryid needs an extra round trip per statement, the registry would need
  persisting across restarts and resets, and it is invisible to a DBA reading
  pg_stat_statements. The tag is stored with the entry. First-text-wins collisions are
  avoided by running pg_sage under its own role (`sage_agent` in `config.example.yaml`).
- **Conservative splitting.** Two of ~700 entries stay untagged rather than risking SQL
  changes; they reference the sage schema, so only selfcost misses them (startup DDL).
- Out of scope, left as they are: PgBouncer admin console (no pg_stat_statements; a comment
  could break the console), migration rehearsal and vector lab (run on clones/lab DBs),
  `bench export-replay` CLI (reads only sage tables, caught by the schema rule).

## Test Results

**Command:** `go test -cover -count=1 -p 2 ./...` (PG17 `pgsage-ag5` :55475)
**Total:** 99 packages ok, 0 failed. Touched packages, `-v` on shared PG14 / PG18 matrix:
1240 / 1241 passed, 0 failed, 1 / 0 skipped. `-race` on touched packages (own PG18): pass.
e2e (`-tags=e2e`, ag5): 78 passed, 0 failed, 13 skipped. Perf gate (`perfgate`,
`PG_SAGE_PERF_SCALE=small`): pass, 0 offenders on ag5 (PG17) and on PG18.
Lint: `golangci-lint run ./...` 0 issues.

**Coverage (touched):** selfmonitor 93.5%, api 79.2%, selfcost 95.9%, workload 97.2%.

### Skipped Tests (must be zero or justified)
- internal/api: TestActionLogCappedCountWalksTheTimeIndex (PG14 only): EXPLAIN
  (GENERIC_PLAN) requires PostgreSQL 16+.
- e2e: 13 live-LLM tests (TestLLM*, TestTunerLLM_*, TestOptimizerMultiQueryConsolidation):
  `SAGE_LLM_API_KEY` not set (live LLM tests are opt-in by design).

### Failures
None.

### Coverage Gaps
All touched packages meet thresholds. Below 70% in the full run, untouched: cmd/create_admin
51.5%, cmd/reset_admin_for_test 50.0%, cmd/sigstore_trusted_root 56.1%,
testsupport/pgssepoch 58.3%, sre-bench 65.7%.

### Bugs Found This Session
1. [BUG] tagplace.go: a parenthesized statement got its tag in front, which PG18 drops.
2. [BUG] tagconn.go: only the first statement of a multi-statement Query was tagged (all
   versions; 249 of ~700 pg_sage entries untagged after a start).
3. [BUG] api/database_helpers.go: the connection test ran untagged and without
   application_name pg_sage.
4. [BUG] tagplace.go `keywordEnd`: a tag could split an identifier containing `$` (fuzz).

### Mutation testing
21 mutations of the tagger, splitter and matchers (first-statement-only, leading tag,
anchored `IsTagged`, prefix-anchored selfcost `strpos`, prefix exclusion `ILIKE`,
`Classify` prefix, no paren rule, no depth, no backslash bail, no dollar/identifier/comment
units, plain connect in the API, ...): all killed. Two equivalent mutants (doubled-quote
and `$n` handling) showed dead code, which was removed. Fuzzing (`FuzzPlaceTags`, 5 min):
one finding (bug 4), fixed and seeded.

### Post-test audit
- Untested input: `standard_conforming_strings=off` servers; handled by falling back
  whenever a literal holds a backslash (never splits on doubt).
- The fuzz invariant ignores inputs already holding a pg_sage comment (a moved labelled tag
  reorders text); those are covered by unit cases and idempotence.
- No fake hides the server: the cross-version test reads the real pg_stat_statements and
  evaluates `workload.AdviceSQL` in SQL; the API test captures real wire bytes.

## Left

- `go test -fuzz` does not start in packages whose `TestMain` uses `testdb.Run` (worker
  exits with EOF); fuzzing ran on a copy without it.
