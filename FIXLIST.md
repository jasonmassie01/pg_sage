# pg_sage Fix List

## 1. EXPLAIN fails on parameterized queries (pg_stat_statements $N placeholders)

**Status:** Open
**File:** `pg_sage/src/explain_capture.c` lines 318-357
**Severity:** High — affects most real-world queries

**Problem:**
`sage.explain(queryid)` finds the query text from `pg_stat_statements` but can't run `EXPLAIN` on it because `pg_stat_statements` normalizes literals to `$1`, `$2`, etc. PostgreSQL's planner can't plan a query with unbound parameters, so the `EXPLAIN` throws an error caught by `PG_CATCH()`, logged at `LOG` level (invisible to user), and returns "No plan available."

**Repro:**
```sql
SELECT sage.explain(-3380051341123203291);
-- Query is: SELECT count(*) FROM pg_class WHERE relkind = $1
-- Returns: "No plan available for this queryid."
```

**Fix:**
For PostgreSQL 16+, add `GENERIC_PLAN` to the EXPLAIN options:
```c
appendStringInfo(&sql,
    "EXPLAIN (FORMAT JSON, COSTS, VERBOSE, GENERIC_PLAN) %s",
    query_text);
```
This tells the planner to generate a generic plan without needing actual parameter values.

For PostgreSQL < 16, fall back to replacing `$N` placeholders with `NULL::unknown` or similar castable defaults before running EXPLAIN. Alternatively, use `pg_stat_statements` query text only for display and skip EXPLAIN when parameters are detected.

**Notes:**
- The error is caught silently (`elog(LOG, ...)`) — user only sees "No plan available" with no hint about why. Consider upgrading to `WARNING` or adding a hint about parameterized queries.
- Check server version at runtime with `PG_VERSION_NUM >= 160000` to pick the right strategy.

## 2. Walkthrough references `curl localhost:9187/metrics` without mentioning sidecar prerequisite

**Status:** Fixed — new platform-specific walkthroughs created, original updated
**File:** `docs/walkthrough.md`
**Severity:** Medium — confusing for anyone following the walkthrough

**Problem:**
The walkthrough includes a step to `curl -s http://localhost:9187/metrics` but never mentions that port 9187 is served by the Go sidecar (`sidecar/main.go`), not the PostgreSQL extension. Users following the walkthrough with only the extension installed get an empty response with no explanation.

**Fix:**
Either:
1. Add a prerequisite section to the walkthrough explaining that the sidecar must be built and running (`go build -o sage-sidecar . && ./sage-sidecar`) before the metrics step, **or**
2. Move the metrics/sidecar section into its own walkthrough and clearly label it as requiring Go / Docker, **or**
3. Add a note inline before the `curl` step: _"This requires the Go sidecar to be running — see `sidecar/README.md`."_

## 3. ReAct diagnostic loop crashes on LLM-generated bad SQL

**Status:** Fixed — subtransaction pattern replaces nested SPI
**File:** `pg_sage/src/briefing.c` lines 1033-1068
**Severity:** Critical — crashes the psql session

**Problem:**
When `sage.diagnose()` asks the LLM to investigate, the LLM may generate SQL with wrong column names (e.g., `indexname` instead of `sui.indexrelname`). The ReAct loop is supposed to catch query errors gracefully and feed them back to the LLM for correction. Instead, the nested `SPI_connect()` / `SPI_finish()` inside `PG_TRY/PG_CATCH` corrupts memory, causing:

1. `pfree called with invalid pointer` — double-free or dangling pointer
2. Resource leaks: unclosed relations, cache references, snapshot references
3. `server sent data ("D" message) without prior row description ("T" message)` — corrupted protocol state
4. The psql session becomes unusable

**Repro:**
```sql
SELECT sage.diagnose('Which indexes on the orders table are wasting resources?');
-- LLM generates: SELECT ... indexname AS index_name ... FROM pg_stat_user_indexes
-- Error: column "indexname" does not exist
-- Then: pfree crash, resource leaks, broken session
```

**Root Cause:**
Lines 1033-1068 open a nested SPI connection to execute the LLM's diagnostic SQL. When the query fails, PostgreSQL's error recovery (`PG_CATCH`) rolls back transaction state, invalidating pointers still held by the outer SPI context. The `EmitErrorReport()` + `FlushErrorState()` at line 1054-1055 doesn't fully restore the SPI stack, and the subsequent `SPI_finish()` operates on corrupted state.

**Fix Options:**
1. **Use SAVEPOINTs instead of nested SPI**: Replace the nested `SPI_connect()/SPI_finish()` with `SAVEPOINT _sage_diag` / `ROLLBACK TO SAVEPOINT` (the same pattern used at line 572 for briefing storage). This keeps one SPI connection and isolates query failures properly.

```c
/* Instead of nested SPI_connect: */
SPI_execute("SAVEPOINT _sage_diag", false, 0);
PG_TRY();
{
    SPI_execute("SET LOCAL transaction_read_only = true", false, 0);
    sql_result = spi_query_simple(diag_sql);
    SPI_execute("RELEASE SAVEPOINT _sage_diag", false, 0);
}
PG_CATCH();
{
    FlushErrorState();
    SPI_execute("ROLLBACK TO SAVEPOINT _sage_diag", false, 0);
    SPI_execute("RELEASE SAVEPOINT _sage_diag", false, 0);
    sql_result = pstrdup("[Query execution error]");
}
PG_END_TRY();
```

2. **Add LLM system prompt guardrails**: Enhance the system prompt to specify correct column names for common catalog views (e.g., "pg_stat_user_indexes uses `indexrelname`, not `indexname`"). This reduces but doesn't eliminate the issue.

3. **Pre-validate generated SQL**: Before executing, check for obvious errors (parse with `SPI_prepare` in a savepoint, discard if it fails).

**Notes:**
- The briefing storage code at line 572 already uses the SAVEPOINT pattern correctly — the diagnostic loop should follow the same approach.
- This is the #1 user-facing bug since `sage.diagnose()` is the flagship LLM feature.

## 4. Sidecar: SQL injection via string interpolation

**Status:** Fixed — all queries parameterized with $1 placeholders
**Files:** `sidecar/resources.go`, `sidecar/tools.go`
**Severity:** High — security vulnerability

**Problem:**
`readSchema`, `readStats`, `readExplain`, and `toolSuggestIndex` used `fmt.Sprintf('%s', sanitize(x))` to embed values directly into SQL. While `sanitize()` stripped dangerous characters, this is a fragile anti-pattern.

**Fix:**
Converted all queries to use `$1` parameterized placeholders via pgx. Updated `queryJSON` and `queryJSONFallback` to accept variadic args.

## 5. Sidecar: 30s hard timeout kills LLM-backed operations

**Status:** Fixed — per-tool timeouts + 180s safety net
**Files:** `sidecar/main.go`, `sidecar/tools.go`
**Severity:** Critical — diagnose/suggest_index/review_migration silently failed

**Problem:**
`requestTimeoutMiddleware` applied a blanket 30s context timeout. LLM-backed tools like `diagnose` routinely need 45-120s.

**Fix:**
Raised middleware timeout to 180s as a safety net. Added `toolTimeouts` map in `tools.go` giving each tool its own context timeout (120s for LLM-backed tools).

## 6. Sidecar: SSE responses silently dropped when buffer full

**Status:** Fixed — increased buffer + blocking write with timeout
**File:** `sidecar/main.go`
**Severity:** Critical — large responses (findings, suggest_index) never reached client

**Problem:**
SSE channel buffer was 64 items with a non-blocking `select default` that silently dropped responses. Any slow SSE reader would lose data.

**Fix:**
Increased buffer to 256. Replaced instant-drop `select default` with a blocking write + 10s timeout + session-closed detection.

## 7. Sidecar: explain resource crashed on NULL return + wrong column name

**Status:** Fixed
**File:** `sidecar/resources.go`
**Severity:** High — `sage://explain/{queryid}` always returned error

**Problem:**
1. `sage.explain_json()` returns NULL when no plan is cached — pgx can't scan NULL into `*string`.
2. Fallback query referenced `plan` column but the actual column is `plan_json`.

**Fix:**
Wrapped primary with `coalesce(..., '{"error":"no cached plan"}')`. Fixed fallback column to `plan_json` and added `::bigint` cast for queryid parameter.

## 8. Sidecar: connection pool too small + no lifecycle management

**Status:** Fixed
**File:** `sidecar/main.go`
**Severity:** High — 5 concurrent slow queries exhausted pool

**Problem:**
Default `PGMaxConns=5`, `PGMinConns=1`. No `MaxConnLifetime`, `MaxConnIdleTime`, or `HealthCheckPeriod` configured.

**Fix:**
Bumped defaults to `MaxConns=10`, `MinConns=2`. Added `MaxConnLifetime=30m`, `MaxConnIdleTime=5m`, `HealthCheckPeriod=30s`.

## 9. Sidecar: fallback queries swallowed error context

**Status:** Fixed
**Files:** `sidecar/resources.go`, `sidecar/tools.go`
**Severity:** Medium — impossible to debug which resource/tool failed

**Problem:**
`queryJSONFallback` and `queryTextFallback` formatted errors as `"primary: ...; fallback: ..."` with no component name.

**Fix:**
Added `component` parameter to both functions. All callers now pass context like `"resource:explain"`, `"tool:diagnose"`. Primary failures are logged at WARN, both-fail at ERROR.

## 10. Sidecar: URI validation used fragile manual slicing

**Status:** Fixed
**File:** `sidecar/main.go`
**Severity:** Medium — edge cases bypassed validation

**Problem:**
`validateResourceURI` used `len(uri) > N && uri[:N]` instead of `strings.HasPrefix`. Empty table/queryid in URI fell through to "unknown resource" instead of a specific error.

**Fix:**
Rewrote with `strings.HasPrefix`/`strings.TrimPrefix` + explicit empty-value checks with helpful error messages.

## 11. Sidecar: dead TokenBudget config field

**Status:** Fixed — removed
**File:** `sidecar/main.go`
**Severity:** Low — dead code

**Problem:**
`Config.TokenBudget` was loaded from `SAGE_TOKEN_BUDGET` env but never referenced. `tokens_used` in `mcp_log` was always hardcoded to 0.

**Fix:**
Removed `TokenBudget` field from `Config` struct and `loadConfig()`.
