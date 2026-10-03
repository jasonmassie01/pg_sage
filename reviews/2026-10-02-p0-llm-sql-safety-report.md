# Phase 0 LLM and SQL safety: report (2026-10-02)

Branch `claude/p0-llm-sql-safety` (from master v1.8.1, `9c52624`). Scope: Phase 0 items 2, 3, 4
and 9 of `reviews/2026-10-02-ai-next/ROADMAP.md`, plus the LLM-input hygiene from
`tuning.md` (I11, I12) and the small MCP conformance fixes from `interfaces.md` (I6, I9).
Commits are tests first, then implementation; nothing is pushed.

## What was built

### 2. `/api/v1/explain`: ANALYZE only when proven side-effect free

EXPLAIN ANALYZE executes the query. A READ ONLY transaction blocks table writes but not
volatile functions, so before this change `SELECT pg_terminate_backend(pid) FROM
pg_stat_activity` sent to `/explain` killed sessions. Now:

- `sqlast.InspectReadQuery` (new, `sqlast/readquery*.go`) parses the statement with
  libpg_query and lists everything execution could invoke: function calls (including
  aggregates and FROM functions), `x.name` attribute notation (PostgreSQL treats it as
  `name(x)`), operators (including those implied by IN, BETWEEN, LIKE, simple CASE,
  GREATEST/LEAST, ANY/ALL sublinks), cast targets, relations and CTE names, plus FOR
  UPDATE/SHARE, data-modifying statements anywhere, and SELECT INTO.
- `explain/analyze_guard.go` + `analyze_catalog.go` resolve those names on the same
  connection, inside the explain transaction (so the session's search_path applies), in four
  batched catalog queries:
  - functions: every overload the name could resolve to in `current_schemas(true)` (or the
    named schema) must be `provolatile IN ('i','s')`; aggregates also have their support
    functions checked (pg_proc records every aggregate as IMMUTABLE);
  - attribute notation: refused only if a volatile function of that name exists;
  - operators: the operator's `oprcode` must be immutable/stable;
  - cast targets: no domains (CHECK constraints run on coercion), no volatile input/receive
    or cast function;
  - relations: views are expanded with `pg_get_viewdef` and checked recursively (depth 4,
    16 views per request); foreign tables and tables with row-level security are refused.
  - A denylist covers STABLE functions that run SQL text or read relations named only by an
    argument (`query_to_xml`, `table_to_xml`, `schema_to_xml`, `database_to_xml` families,
    `ts_stat`, `pg_input_is_valid`, `pg_input_error_info`) and anything named `dblink*`.
  - Anything that does not resolve is refused ("unknown → refuse"); a catalog error refuses
    with its cause. The check runs inside a savepoint so a failed lookup cannot abort the
    transaction the EXPLAIN still needs. Without cgo the parser is unavailable and ANALYZE
    is always refused.
- A refused ANALYZE returns the plan-only EXPLAIN with `analyze_refused` (new response
  field) and a `note` that says why. Plan-only EXPLAIN stays allowed for everything that
  passes the existing single-read-statement allowlist.
- `explain.timeout_ms <= 0` now means the default (10 s) for both the request deadline and
  `SET LOCAL statement_timeout`; before, 0 set an unlimited statement_timeout and a negative
  value failed the request. Sub-millisecond values round up to 1 ms, never 0.
- The cache key includes the mode (`cacheKey`), so a plan-only result never answers an
  ANALYZE request.
- When an enabled LLM fails (error, 429, timeout, empty or unparsable reply) the
  deterministic fallback is cached for 1 minute instead of the full TTL. No LLM configured
  keeps the full TTL.

### 3. LLM input hygiene

- `llm.neutralizeDataTags` scans the original bytes with ASCII-only case folding. The old
  code took offsets from `strings.ToLower(text)`, which changes byte length for U+212A and
  U+0130: it could panic (slice out of range) or let `</data` through.
- `llm.redactQueryValue` (provider error redaction) uses the same original-string scan.
  Non-ASCII look-alikes of the key (`Key=`) are not treated as the key.
- `llm/sanitize.go`: E-strings honour backslash escapes (`E'\' OR pw = '` no longer exposes
  the next literal); `StripSQLComments` and `RedactSQLLiterals` respect dollar quotes and
  quoted identifiers (`"it's"` no longer opens a literal); an identifier that merely ends
  in E (`DATE'...'`) is not an E-string; `a$b$` is an identifier, not a dollar quote; an
  unterminated `"` does not shield the rest of the text.
- `SanitizeForLLM` on plan JSON sanitizes each JSON string value (decoded, so `\"` is a
  real quote), because there double quotes delimit JSON strings, not identifiers. Text that
  only looks like JSON (truncated) is scanned with double quotes as plain bytes. Found by
  the existing `TestSanitizePlanForLLM_*` test after the identifier change.
- Fuzz targets with committed seed corpora (`internal/llm/testdata/fuzz/`):
  `FuzzNeutralizeDataTags`, `FuzzRedactQueryValue`, `FuzzSanitizeForLLM`.

### 3b. Plan narrator and action justifier

- `analyzer/plan_narrative.go`: query text, node changes and both plan summaries go to the
  model inside `<data>` blocks, comment-stripped and literal-redacted; the system prompt
  carries `llm.UntrustedDataRule`. The narrative is stored only in `Detail["narrative"]`;
  the deterministic `Recommendation` is no longer overwritten. Failures log at `WARN`
  (it passed `"analyzer"` as the level, which the wrapper printed as INFO).
- `executor/justify.go`: object, title and reason are wrapped as untrusted data; executed and
  rollback SQL go through `SanitizePromptSQL`; the system prompt carries the rule.

### 4. Destructive executor targets must be schema-qualified

`sqlast.Check` (cgo) and the executor's text layer (`checkProtectedSchemaUsage`) refuse an
unqualified `DROP INDEX` or `ALTER TABLE` target. An unqualified name resolves through the
session search_path when it runs (`"$user"` can be `sage`; `pg_catalog` is always searched
first), which the protected-schema check cannot see. For three-part names the schema is
the second-to-last part (previously `db.sage.idx` was checked against `db`). `pg_toast` was
added to the executor's protected schemas. A real-PG test sets `search_path = sage, public`,
shows the unqualified name resolves into `sage`, and checks `ExecConcurrently` refuses it and
leaves the index; a qualified non-protected drop still runs.

### 9. MCP stdio and the briefing

- `briefing.Worker` writes the `stdout` channel to stderr when `mcp.enabled` and
  `mcp.transport: stdio`, and logs that once per worker (`sync.Once`).
- MCP: `ping` returns `{}`; requests without an id and any `notifications/*` method get no
  response (stdio writes nothing; HTTP answers 202 with no body); tool results carry
  `content: [{type: "text", text: <JSON>}]` and keep `structuredContent`.

## Product decisions

1. **Refused ANALYZE degrades to plan-only, with the reason, instead of an error.** The
   caller still gets a plan (what most of them want) and a clear statement that it is
   estimate-only and why. Executing nothing unsafe is the invariant; failing the request
   would add no safety.
2. **Fail closed and stay conservative.** Unknown names, catalog errors, parse failures,
   builds without cgo, views nested deeper than 4, foreign tables and RLS tables all refuse
   ANALYZE. An unqualified function name is refused if any overload in any search_path
   schema is volatile, even when PostgreSQL would pick a safe one (e.g. a volatile
   `public.upper` refuses `upper('x')`). Over-refusal costs timing data; under-refusal can
   terminate sessions.
3. **Trust `provolatile` as declared.** A function created STABLE that has side effects is
   the creator's lie; `/explain` cannot create functions (read-only). The SQL-running STABLE
   built-ins are denylisted explicitly.
4. **Qualification is required, not resolved.** Resolving the name through the catalog at
   validation time would race with the session that executes it and depends on that
   session's search_path. Every pg_sage producer of DROP INDEX / ALTER TABLE already
   qualifies (optimizer `DropDDL`, analyzer `dropIndexSQL`, verified-index rollback,
   migration planner); operator-typed and LLM-written unqualified statements are now refused
   with "target must be schema-qualified". `VACUUM`, `ANALYZE`, `REINDEX` and `CREATE INDEX`
   keep accepting unqualified names (not destructive).
5. **Narratives are evidence, not instructions.** The plan narrative is model prose about
   database content; it now sits beside the deterministic recommendation instead of
   replacing it.
6. **Briefing on stdio MCP goes to stderr, not nowhere.** Operators who configured `stdout`
   still see it in container logs; the JSON-RPC stream stays clean.
7. **JSON plans are sanitized structurally.** The alternative (one regex scanner for SQL
   and JSON) cannot be right for both `"it's"` in SQL and `"(a = 'pii')"` in JSON.

## Spec CHECKs

No `AI-SRE-SPEC.md` CHECK covers these items directly; they are the roadmap's Phase 0
safety items 2, 3, 4 and 9. CHECK-39 (MCP role enforcement) is unchanged and its tests
still pass with the new response shape.

## Test Results

**Commands (Docker `golang:1.25`, `--cpus=2`):**
- `go test -p 2 -count=1 -cover -timeout 2400s ./...` on PG17 (`pgsage-ag2`, :55472)
- `go test -p 2 -race -tags=integration -count=1 -cover <touched>` on PG17
- `go test -p 2 -tags=integration -count=1 -v <touched>` on PG14 (:55414) and PG18 (:55418)
- touched = sqlast, explain, llm, analyzer, executor, mcp, briefing
- 60 s of `go test -fuzz` per fuzz target; mutation runs per package (below)

**Total:** full suite on PG17: 76 packages ok, 2 failed (`cmd/pg_sage_sidecar`
`TestComposedSRE_DetectorEpisodeOpensAnIncidentCaseAndInvestigation`, `internal/api`
`TestEmergencyStopRecordsSessionActor`, a bootstrap advisory-lock timeout). Both pass when
re-run alone, touch none of the changed code, and an earlier full run under the same load
failed a different set (connection timeouts, `sre_investigations_active_ms_check`), which
also passed on re-run. Touched packages with `-race -tags=integration` on PG17: all pass, no
data races. PG18: all pass. PG14: all pass except `TestApplyLockCeilingCapsCycleAnalyze`
(the uncapped control lock wait ended at ~2.06 s instead of ~4 s). It runs ANALYZE through
the lock-ceiling path, which this branch does not change; it passed `-count=4` in isolation
on PG14 both on this branch and on master `9c52624`, so it is a load-sensitive timing test.
76 new test/fuzz functions; 4 existing tests changed (see "Test changes").

**Coverage (touched packages, PG17 `-race -tags=integration`):**

| Package | Coverage |
|---|---|
| internal/sqlast | 97.8% |
| internal/briefing | 95.0% |
| internal/explain | 93.8% (91.2% without the integration tag) |
| internal/llm | 93.7% |
| internal/analyzer | 86.9% |
| internal/executor | 83.9% |
| internal/mcp | 79.9% |

All touched packages meet their thresholds (70% for business logic).

### Skipped Tests
- internal/llm: `TestChatWithToolsLive_RealProvider`, `TestChatLive_RealProvider`: live LLM
  tests, run only with `PG_SAGE_LIVE_LLM=1` (rules: no live LLM by default).
- No other skips in the touched packages (from the `-v` runs on PG14/PG18). The full-suite
  run was not verbose, so skips outside the touched packages are not listed here.

### Failures
- None caused by this branch. The flaky tests above are listed under Total.

### Coverage Gaps
- None below threshold. `internal/mcp` is the lowest at 79.9% (unchanged by this branch).

### Test changes (tests corrected, not weakened)
- `executor_new_test.go`, `validate_test.go`, `wave1_policy_test.go`,
  `index_verification_runtime_test.go`: fixtures that asserted unqualified `DROP INDEX` /
  `ALTER TABLE` are accepted now use `public.<name>`; that acceptance is the policy Phase 0
  #4 removes, not the behavior those tests check (commit `e24c332`).
- `plan_narrative_hygiene_test.go`: an empty completion is an error in `llm.Client`, so the
  narrator logging WARN is correct; my first expectation (silent) was wrong (`a3d3f7d`).

### Mutation testing
22 mutations of the key logic, each run against its package's tests on PG17: all 22
killed. They covered: ASCII folding in tag neutralization; the original-string scan in key
redaction; E-string escapes; quoted-identifier handling; the plan-JSON path;
DROP INDEX qualification and the 3-part schema index; the executor text-layer check; the
function volatility filter; view recursion; the cache-key mode; timeout <= 0; the
fallback TTL; MCP notifications; the briefing redirect; the narrator Recommendation
overwrite; the justifier and narrator sanitizing; the `table_to_xml` denylist; the
FOR UPDATE refusal; attribute-notation collection; and the domain cast check. Mutation
first exposed one gap: the executor text-layer check was masked by the parse-tree layer
in cgo builds. `TestCheckProtectedSchemaUsageRequiresQualification` was added
(commit `e6cb21d`) and kills it.

## Bugs found
1. [BUG] `explain/explain.go` prepareConn: `timeout_ms: 0` set `statement_timeout = '0ms'`
   (no server-side limit; only the 10 s client context deadline remained) and a negative
   value failed every request.
2. [BUG] `explain`: volatile functions executed under EXPLAIN ANALYZE (from the code: the
   only guard was the READ ONLY transaction). The real-PG test now asserts that a victim
   session survives `pg_terminate_backend`, called directly and through nested views.
3. [BUG] `explain_cache.go`: plan-only and ANALYZE shared a cache key.
4. [BUG] `llm/untrusted.go`, `llm/models.go`: lower-cased offsets used on the original
   string (panic and tag escape with U+212A / U+0130).
5. [BUG] `llm/sanitize.go`: `E'\' ...` exposed the following literal; `"it's"` desynced the
   scanner and exposed the next literal; `DATE'...'` lost its `E` in the output (the last
   from code reading).
6. [BUG] `sqlast/check_cgo.go`: unqualified DROP INDEX skipped the protected-schema check,
   and in `db.schema.idx` the database name was checked as the schema.
7. [BUG] `analyzer/plan_narrative.go`: logged failures with level `"analyzer"` (printed as
   INFO) and replaced the deterministic recommendation with model prose.
8. [BUG] `briefing.go`: the stdout channel wrote into the MCP stdio JSON-RPC stream.
9. [BUG] `mcp/server.go`: answered notifications (a stray stdio line) and had no `ping`.

## Post-test audit
- **Inputs not tested:** domain CHECK constraints reached indirectly (`jsonb_populate_record`
  into a composite with a domain field); user-defined types whose btree/hash support
  functions are volatile (used implicitly by ORDER BY / DISTINCT / joins); foreign
  partitions under a local partitioned parent; STABLE user functions with side effects
  (trusted by design). All need a pre-existing, unusual object created by a privileged
  user; `/explain` itself cannot create objects.
- **Assertions that could pass while broken:** the fake-PG unit tests only prove that a
  catalog error fails closed. The resolution SQL itself is covered only by the real-PG tests
  (PG14, 17 and 18), which assert both refusals and that safe queries still get ANALYZE.
- **Fakes that hide failures:** the OpenAI-compatible httptest server stands in for the LLM
  (rules); the 429 path is bounded by the client timeout rather than the full 1+4+16 s
  retry ladder.
- **Over-refusal is by design:** a qualified column named like a volatile function
  (`t.random`) and any unqualified call with a volatile overload anywhere on the
  search_path refuse ANALYZE.

## What is left
- Plan narratives are stored in `Detail.narrative`, but the findings UI does not render
  them as a separate field yet (they show in the raw detail view).
- Not in this scope: narrator caching per `(queryid, plan pair)` (I11); MCP `isError` tool
  results, `resources/list`, `database` args and the `apply_migration` schema (I6); briefing
  through `notify` with the SSRF guard and UI storage (I9).
- Coordinator decision: operator-typed unqualified `DROP INDEX` / `ALTER TABLE` (manual
  actions, MCP intents) are now refused with "target must be schema-qualified". Every pg_sage
  producer already qualifies. If the UI should qualify automatically, that is a follow-up.
- The full suite shows load-sensitive timing flakes (listed above) while other agents share
  the Docker VM; they are not related to this branch.
