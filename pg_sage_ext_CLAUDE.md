# CLAUDE.md — pg_sage C Extension (v0.5.0 → v0.6.0)

## Mission

Apply v1 test report fixes to the pg_sage C extension, wire up LLM integration to Gemini (if implemented), and prepare a test-ready build. When complete, all 4 PG versions (14-17) should pass the v2 adversarial test plan with Tier 3 execution, auto-explain, and LLM-enhanced analysis all active.

---

## PRE-BUILD: Git Workflow

```bash
cd pg_sage
git checkout master && git pull origin master
git checkout -b fix/ext-v0.6

# Commit sequence:
# 1. "fix: SPI error handling during DROP/CREATE cycling (EXT-FIX-1)"
# 2. "fix: exclude sage schema from all analyzer catalog queries (EXT-FIX-2)"
# 3. "feat: toast_bloat_min_rows GUC for TOAST bloat threshold (EXT-FIX-3)"
# 4. "feat: schema_design min_rows/min_columns GUC to reduce noise (EXT-FIX-3b)"
# 5. "feat: LLM integration with Gemini via libcurl (EXT-FIX-6) — OR docs: document LLM as sidecar-only"
# 6. "test: auto-explain end-to-end verification (EXT-FIX-5)"
# 7. "chore: GUC documentation and defaults audit"

# After all work:
git tag v0.6.0-rc1
git push origin fix/ext-v0.6
git push origin v0.6.0-rc1
```

---

## PRE-BUILD: V1 Test Report Analysis

### What v1 proved

11/11 phases PASS. 2 bugs found. Extension builds, runs, and survives crash recovery on all 4 PG versions. Background workers restart after kill -9. GUCs work. Advisory lock works. Circuit breaker works. Trust ramp correctly blocks premature Tier 3 execution.

### What v1 did NOT test (blind spots)

| Blind Spot | Impact |
|-----------|--------|
| Tier 3 never executed | Trust ramp blocked all actions. Zero entries in action_log. We have no idea if CREATE INDEX CONCURRENTLY, DROP INDEX, VACUUM, or pg_terminate_backend actually work when the executor fires. |
| LLM never connected | Not mentioned in the report at all. Either the feature doesn't exist in C, or it was never enabled. |
| Auto-explain never enabled | Phase 4.3 was PARTIAL. `sage.autoexplain_enabled=off` (correct default), but the test never flipped it on. The full capture → storage → retrieval path is untested. |
| Snapshot asymmetry | PG14: 12 snapshots. PG17: 85 snapshots. Same machine, same test window. 7x difference invalidates all cross-version comparisons. |
| Schema self-analysis | Sidecar had FIX-1: sage analyzing its own FK constraints. Extension shows `sage_health: warning: 1` (intentional) but 204 `schema_design` findings on 220 tables may include sage's own 7 tables. |
| 1 slow query finding despite 10+ slow queries run | TPC-H Q1/Q3/Q5/Q9 at ~3700ms each + suboptimal + pathological queries. Only 1 slow_query finding. Either collector missed them or they ran before collection started. |
| 204 schema_design findings is noise | Almost every table flagged. Real design issues are invisible in this volume. |
| 37 TOAST bloat warnings | Many likely from edge case tables (wide500 etc.). Same class of noise as sidecar's `nation` table false positive. |

### Bugs found in v1

| # | Bug | Severity | Status |
|---|-----|----------|--------|
| 1 | Workers error during rapid DROP/CREATE cycling (`relation "sage.snapshots" does not exist`) | Low | Open — workers recover but log noise |
| 2 | trust_level GUC accepts invalid string values | Medium | FIXED — check_hook added |

---

## PRE-BUILD: Diagnostics (Run BEFORE Coding)

Run these on the EXISTING test clusters if still running. If torn down, re-create fresh clusters with v0.5.0 and re-run the v1 workload to reproduce.

### Diagnostic 1: Sage schema self-analysis (determines EXT-FIX-2 scope)

```sql
-- Run on PG17 (highest finding count):
SELECT category, object_identifier, severity, detail
FROM sage.findings
WHERE object_identifier LIKE 'sage.%'
  AND category NOT IN ('sage_health')
ORDER BY category, object_identifier;
```

**If rows returned:** Extension IS analyzing its own schema. Same bug as sidecar FIX-1. EXT-FIX-2 is a real bug fix.
**If zero rows:** Extension already excludes sage schema. EXT-FIX-2 is a preventive measure (still add the filter for safety, but lower priority).

**Secondary check:** 220 tables total - 7 sage tables = 213 user tables. If schema_design findings reference more than 213 distinct `object_identifier` values, sage tables are leaking through.

### Diagnostic 2: Snapshot asymmetry root cause

```sql
-- Run on PG14 (lowest snapshot count = 12):
SELECT collected_at,
       collected_at - lag(collected_at) OVER (ORDER BY collected_at) AS gap
FROM sage.snapshots
WHERE category = 'queries'
ORDER BY collected_at;
```

**If gaps > 5 minutes exist:** Circuit breaker is firing or collector is stalling. Check:
```sql
SELECT sage.health_json();
-- Look for: circuit_state, consecutive_skips, last_skip_reason
```

**If no gaps but only 12 snapshots:** PG14 started collecting much later than PG17. The test procedure didn't start all clusters simultaneously.

**If circuit breaker is firing on PG14:** PG14 may have higher baseline load from something version-specific. Investigate `active_backends` count.

### Diagnostic 3: Slow query detection gap

```sql
-- Run on PG17 (85 snapshots, best chance of capture):
SELECT DISTINCT elem->>'queryid' AS queryid,
       (elem->>'mean_exec_time')::float AS mean_exec_ms,
       left(elem->>'query', 80) AS query_preview
FROM sage.snapshots
CROSS JOIN LATERAL jsonb_array_elements(data) AS elem
WHERE category = 'queries'
  AND (elem->>'mean_exec_time')::float > 500
ORDER BY mean_exec_ms DESC;
```

**Expected:** 5+ distinct queryids (TPC-H Q1/Q3/Q5/Q9 + at least 1 pathological)
**If 0-1:** Queries ran before collector started, OR pg_stat_statements wasn't capturing during workload application.

### Diagnostic 4: Schema design finding targets

```sql
SELECT object_identifier, severity, count(*) AS occurrence_count
FROM sage.findings
WHERE category = 'schema_design'
GROUP BY object_identifier, severity
ORDER BY count(*) DESC LIMIT 30;
```

**Check:** Are sage.* tables in this list? Are the 200 partition children each generating individual findings (that's 200 findings from partitions alone)?

### Diagnostic 5: LLM feature existence

```sql
SHOW sage.llm_enabled;
```

**If error "unrecognized configuration parameter":** LLM is NOT implemented in the C extension. Skip EXT-FIX-6. Document as sidecar-only feature.
**If returns a value (on/off):** LLM code exists. Proceed with EXT-FIX-6.

---

## Fix 1: SPI Error Handling During DROP/CREATE Cycling (EXT-FIX-1)

**Priority:** P2
**Bug:** #1 from v1 test
**Files:** The actual filenames depend on the codebase structure. Find them:

```bash
# Find all background worker source files:
grep -rn "BackgroundWorkerMain\|bgw_main\|RegisterBackgroundWorker\|worker_main" src/ --include="*.c" -l
# Find the collector, analyzer, and briefing worker entry points:
grep -rn "collector\|analyzer\|briefing" src/ --include="*.c" -l
# The fix must be applied to ALL worker files that call SPI functions.
```

**Root cause:** Background workers are registered via `shared_preload_libraries` and survive `DROP EXTENSION`. They continue trying to INSERT into `sage.snapshots`, `sage.findings` etc. during the window where the extension schema doesn't exist.

**Fix:** Wrap each worker's main loop body in PG_TRY/PG_CATCH with a schema existence pre-check. The code below is a PATTERN — adapt to the actual function names and variable names in each worker file:

```c
/*
 * PATTERN: Apply this to the main loop body in each worker.
 * Replace 'worker_name' with the actual worker identifier string.
 * Replace 'do_worker_cycle()' with the actual work function (e.g., collect_snapshots(), run_analyzer(), generate_briefing()).
 * The PG_TRY/PG_CATCH structure goes AROUND the existing transaction + SPI block.
 */
PG_TRY();
{
    SetCurrentStatementStartTimestamp();
    StartTransactionCommand();
    SPI_connect();
    PushActiveSnapshot(GetTransactionSnapshot());

    /* Fast path: check schema exists before any writes */
    int ret = SPI_execute(
        "SELECT 1 FROM pg_namespace WHERE nspname = 'sage'",
        true,  /* read_only */
        1      /* max rows */
    );

    if (SPI_processed == 0)
    {
        elog(LOG, "pg_sage %s: sage schema not found, skipping cycle", worker_name);
        SPI_finish();
        PopActiveSnapshot();
        CommitTransactionCommand();
        goto next_cycle;
    }

    /* Normal work: collect / analyze / brief */
    do_worker_cycle();  /* REPLACE with actual function name */

    SPI_finish();
    PopActiveSnapshot();
    CommitTransactionCommand();
}
PG_CATCH();
{
    /*
     * IMPORTANT: After an error inside a transaction, the transaction is
     * already in abort state. We must abort it BEFORE trying to clean up SPI.
     * The ordering is: FlushErrorState → AbortCurrentTransaction.
     * Do NOT call SPI_finish() here — SPI state is undefined after an error
     * inside a transaction. AbortCurrentTransaction handles cleanup.
     */
    ErrorData *edata;
    MemoryContext oldcontext = MemoryContextSwitchTo(TopMemoryContext);
    edata = CopyErrorData();
    MemoryContextSwitchTo(oldcontext);

    elog(LOG, "pg_sage %s: SPI error (%s), will retry next interval",
         worker_name, edata->message);

    FlushErrorState();
    FreeErrorData(edata);

    /* Abort the failed transaction — this also cleans up SPI and snapshots */
    AbortCurrentTransaction();
}
PG_END_TRY();

next_cycle:
    /* Sleep until next interval */
```

**Key details:**
- After `AbortCurrentTransaction()`, shared memory state is still valid. The worker just skips one cycle.
- `goto next_cycle` after schema-not-found avoids unnecessary PG_CATCH overhead.
- The schema check query (`pg_namespace`) is itself inside the PG_TRY — if even THAT fails (extremely unlikely, but possible if SPI state is corrupted), the CATCH handles it.
- Do NOT call `SPI_finish()` inside PG_CATCH — after an error, SPI state is undefined. `AbortCurrentTransaction()` handles all cleanup.
- Do NOT use `ereport(ERROR, ...)` inside the CATCH — that re-throws and defeats the purpose.

**Verification test:**
```sql
-- Rapid DROP/CREATE 10x:
DO $$
BEGIN
    FOR i IN 1..10 LOOP
        DROP EXTENSION IF EXISTS pg_sage CASCADE;
        PERFORM pg_sleep(0.5);
        CREATE EXTENSION pg_sage;
        PERFORM pg_sleep(0.5);
    END LOOP;
END $$;
```
After: check PG log. Must contain zero `ERROR` entries from pg_sage workers. Only `LOG` entries reading "sage schema not found, skipping cycle" or "SPI error (...), will retry next interval."

**Edge case:** What if `DROP EXTENSION` is called but `CREATE EXTENSION` never follows? Workers will log "sage schema not found" every cycle indefinitely. That's correct behavior — the extension IS dropped, the workers are just leftover from `shared_preload_libraries`. User must either `CREATE EXTENSION pg_sage` again or remove `pg_sage` from `shared_preload_libraries` and restart.

Commit: `fix: SPI error handling during DROP/CREATE cycling (EXT-FIX-1)`

---

## Fix 2: Sage Schema Exclusion (EXT-FIX-2)

**Priority:** P1
**Files:** `src/analyzer.c`, `src/collector.c` (any file containing catalog query SQL strings)

### Step 1: Audit ALL SQL queries in the C code

```bash
grep -rn "pg_stat_user_tables\|pg_stat_user_indexes\|pg_constraint\|pg_class\|pg_sequences\|pg_stat_activity" src/ --include="*.c" --include="*.h"
```

For EVERY catalog query found, check: does it filter out `sage`, `pg_catalog`, `information_schema` schemas?

### Step 2: Fix every query missing the filter

Common patterns:

```sql
-- Table stats: must have WHERE schemaname NOT IN ('sage', 'pg_catalog', 'information_schema')
-- Index stats: must have WHERE s.schemaname NOT IN ('sage', 'pg_catalog', 'information_schema')
-- FK constraints: must JOIN pg_namespace n ON c.connamespace = n.oid
--                 AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema')
-- Sequences: must have WHERE schemaname NOT IN ('sage', 'pg_catalog', 'information_schema')
```

**IMPORTANT:** Some queries may already filter `pg_catalog` and `information_schema` but NOT `sage`. Check each one individually.

### Step 3: Add analyzer-level filter as defense-in-depth

In the analyzer's rule iteration (`src/analyzer.c`), add a skip check BEFORE evaluating any rule for an object:

```c
static bool
is_excluded_schema(const char *schemaname)
{
    return (strcmp(schemaname, "sage") == 0 ||
            strcmp(schemaname, "pg_catalog") == 0 ||
            strcmp(schemaname, "information_schema") == 0);
}

/* In each rule's loop over objects: */
for (int i = 0; i < table_count; i++)
{
    if (is_excluded_schema(tables[i].schemaname))
        continue;
    /* evaluate rule */
}
```

This ensures no rule — present or future — generates findings about sage's own schema, even if a catalog query filter is accidentally missed.

### Step 4: Verify

After fix, run:
```sql
-- Should return 0 rows (except sage_health):
SELECT * FROM sage.findings
WHERE object_identifier LIKE 'sage.%'
  AND category NOT IN ('sage_health');
```

**Edge case: sage schema objects in `pg_stat_user_indexes`.** Sage's bootstrap creates indexes (`idx_snapshots_time`, etc.). These will appear in `pg_stat_user_indexes` because they're in a user-created schema, not `pg_catalog`. The index stats collector query MUST filter `s.schemaname != 'sage'` or these indexes will be analyzed (and possibly flagged as unused if they have low `idx_scan` counts after a fresh restart).

**Edge case: `pg_stat_user_tables` vs `pg_stat_all_tables`.** If ANY query uses `pg_stat_all_tables` instead of `pg_stat_user_tables`, it will include `pg_catalog` and `information_schema` tables. Audit for this. `pg_stat_user_tables` already filters those, but `sage` schema tables ARE included in `pg_stat_user_tables` because sage is a user schema.

Commit: `fix: exclude sage schema from all analyzer catalog queries (EXT-FIX-2)`

---

## Fix 3: Noise Reduction — TOAST Bloat and Schema Design (EXT-FIX-3)

**Priority:** P2
**Files:** `src/guc.c`, `src/analyzer.c`

### 3a: TOAST bloat minimum rows

37 TOAST bloat warnings on 220 tables. The edge case tables (wide500 with 500 columns, partition children) have TOAST segments that look bloated because they were bulk-loaded then immediately analyzed.

Add GUC:
```c
/* In guc.c */
static int sage_toast_bloat_min_rows = 1000;

DefineCustomIntVariable(
    "sage.toast_bloat_min_rows",
    "Minimum row count for TOAST bloat detection. Tables below this are skipped.",
    NULL,
    &sage_toast_bloat_min_rows,
    1000,       /* default */
    0,          /* min (0 = check all tables) */
    INT_MAX,    /* max */
    PGC_SIGHUP,
    0,
    NULL, NULL, NULL
);
```

In the TOAST bloat rule in `analyzer.c`:
```c
/* Skip tables below minimum row threshold */
if (table->n_live_tup + table->n_dead_tup < sage_toast_bloat_min_rows)
    continue;
```

**Same pattern as sidecar's FIX-2** — identical fix for the identical false positive class.

### 3b: Schema design minimum thresholds

204 schema_design findings makes the entire findings table useless. Users will ignore ALL findings because signal is buried in noise.

Add GUCs:
```c
static int sage_schema_design_min_rows = 100;
static int sage_schema_design_min_columns = 2;

DefineCustomIntVariable(
    "sage.schema_design_min_rows",
    "Skip schema design analysis for tables with fewer rows than this.",
    NULL,
    &sage_schema_design_min_rows,
    100,        /* default: skip empty/tiny tables */
    0, INT_MAX,
    PGC_SIGHUP, 0, NULL, NULL, NULL
);

DefineCustomIntVariable(
    "sage.schema_design_min_columns",
    "Skip schema design analysis for tables with fewer columns than this.",
    NULL,
    &sage_schema_design_min_columns,
    2,          /* default: skip single-column tables */
    1, INT_MAX,
    PGC_SIGHUP, 0, NULL, NULL, NULL
);
```

**Verification target:** After these fixes, the TPC-H schema alone (8 tables, no edge cases) should produce fewer than 50 total findings. If it still produces 200+, the thresholds need adjustment or there's a different noise source.

**Edge case: partition children.** If the schema has 200 partitions and each child generates a separate schema_design finding, that's 200 findings from one logical table. The partition aggregation fix (if present in the C extension) should suppress individual child findings. If partition aggregation is NOT implemented in the C extension: add a note that partitioned tables may generate excessive findings, and provide a GUC to suppress schema_design for partition children:

```sql
-- Check if partition children are the source of noise.
-- Use pg_inherits to get ACTUAL partition children, not a fragile LIKE pattern:
SELECT f.object_identifier, f.severity, f.category
FROM sage.findings f
WHERE f.category = 'schema_design'
  AND f.object_identifier IN (
    SELECT inhrelid::regclass::text FROM pg_inherits
  )
ORDER BY f.object_identifier;
-- If this returns 100+ rows: partition children are the primary noise source.
-- Fix: suppress schema_design findings for tables that appear in pg_inherits as children.
```

Commit: `feat: noise reduction GUCs for TOAST bloat and schema_design (EXT-FIX-3)`

---

## Fix 4: LLM Integration (EXT-FIX-6)

**Priority:** P1 (conditional)

### Step 1: Determine if LLM code exists

```bash
ls src/llm* 2>/dev/null && echo "LLM FILES EXIST" || echo "LLM NOT IMPLEMENTED"
grep -rn "llm\|curl_easy\|CURLOPT\|libcurl" src/ --include="*.c" --include="*.h"
grep -rn "sage.llm" src/guc.c
```

### If LLM code EXISTS:

The extension has `libcurl`-based LLM integration that was never tested. Configure and test it.

**Verify these GUCs exist (add if missing):**

| GUC | Type | Context | Default | Notes |
|-----|------|---------|---------|-------|
| `sage.llm_enabled` | bool | PGC_SIGHUP | off | Master switch |
| `sage.llm_endpoint` | string | PGC_SIGHUP | (empty) | OpenAI-compatible URL |
| `sage.llm_api_key` | string | PGC_SUSET | (empty) | **Use file-based GUC or PGC_SUSET — see API key security section below** |
| `sage.llm_model` | string | PGC_SIGHUP | (empty) | e.g. `gemini-2.5-flash-preview` |
| `sage.llm_timeout_seconds` | int | PGC_SIGHUP | 60 | libcurl CURLOPT_TIMEOUT |
| `sage.llm_token_budget_daily` | int | PGC_SIGHUP | 100000 | Reset at midnight UTC |
| `sage.llm_cooldown_seconds` | int | PGC_SIGHUP | 300 | Circuit breaker cooldown |

**API key security:** The API key must not leak through `pg_settings` or `SHOW ALL`. Options (in order of preference):

1. **File-based key (RECOMMENDED):** Use a GUC `sage.llm_api_key_file` that points to a file containing the key (e.g., `/etc/pg_sage/gemini_key`). The C code reads the file at `_PG_init()` and on SIGHUP reload. The file is readable only by the postgres OS user. The GUC value (a filepath) is harmless to expose.

2. **PGC_SUSET context:** Register the GUC with `PGC_SUSET` context (superuser-set only). This means only superusers can SET or SHOW it. Users with `pg_monitor` role cannot see the value.

3. **GUC_SUPERUSER_ONLY flag:** If available in the target PG versions (check `include/utils/guc.h`):
```c
DefineCustomStringVariable(
    "sage.llm_api_key",
    "Gemini API key",
    NULL,
    &sage_llm_api_key,
    "",
    PGC_SIGHUP,
    GUC_SUPERUSER_ONLY,  /* hides from non-superusers */
    NULL, NULL, NULL
);
```

**Verify after implementation:**
```sql
-- As a non-superuser (e.g., sage_agent):
SHOW sage.llm_api_key;
-- Must NOT show the actual key. Should error or show empty.

-- As superuser:
SHOW sage.llm_api_key;
-- May show the key (acceptable — superuser can see everything anyway)
```

**Verify libcurl call:**
```c
/* Must have: */
curl_easy_setopt(curl, CURLOPT_URL, sage_llm_endpoint);
curl_easy_setopt(curl, CURLOPT_TIMEOUT, sage_llm_timeout_seconds);
curl_easy_setopt(curl, CURLOPT_POSTFIELDS, json_request_body);

/* Headers: */
struct curl_slist *headers = NULL;
headers = curl_slist_append(headers, "Content-Type: application/json");
char auth_header[512];
snprintf(auth_header, sizeof(auth_header), "Authorization: Bearer %s", sage_llm_api_key);
headers = curl_slist_append(headers, auth_header);
curl_easy_setopt(curl, CURLOPT_HTTPHEADER, headers);

/* Response handling: */
curl_easy_setopt(curl, CURLOPT_WRITEFUNCTION, response_callback);
curl_easy_setopt(curl, CURLOPT_WRITEDATA, &response_buffer);
/* response_buffer must have a size cap (1MB max) */

/* After curl_easy_perform(): */
long http_code = 0;
curl_easy_getinfo(curl, CURLINFO_RESPONSE_CODE, &http_code);
if (http_code != 200)
{
    elog(WARNING, "pg_sage LLM: HTTP %ld from %s", http_code, sage_llm_endpoint);
    /* increment circuit breaker failure count */
}

/* CRITICAL: curl_slist_free_all(headers) and free response_buffer after use */
/* Memory leak in a background worker = slow RSS growth over days */
```

**Gemini configuration for testing:**
```sql
ALTER SYSTEM SET sage.llm_enabled = on;
ALTER SYSTEM SET sage.llm_endpoint = 'https://generativelanguage.googleapis.com/v1beta/openai/chat/completions';
ALTER SYSTEM SET sage.llm_api_key = '${SAGE_GEMINI_API_KEY}';
ALTER SYSTEM SET sage.llm_model = 'gemini-2.5-flash-preview';
ALTER SYSTEM SET sage.llm_timeout_seconds = 60;
SELECT pg_reload_conf();
```

**Verification checklist (all must pass):**
- [ ] `SHOW sage.llm_enabled` returns `on` after `pg_reload_conf()`
- [ ] `sage.health_json()` shows LLM status (enabled, model, circuit state)
- [ ] Within 1-2 analyzer cycles (~20 min): findings with LLM-enhanced `detail` JSONB appear
- [ ] `sage.health_json()` shows `llm_calls_total > 0` and `llm_tokens_used > 0`
- [ ] Set `sage.llm_endpoint` to garbage URL → LLM errors in log, circuit breaker opens
- [ ] Set `sage.llm_token_budget_daily = 100` → budget exhausted after 1-2 calls
- [ ] API key not visible to non-superuser: `SET ROLE sage_agent; SHOW sage.llm_api_key;` must fail or return empty

**Edge case: libcurl not linked.** If the extension was built without `-lcurl`, LLM calls will fail at runtime with a missing symbol error. Check:
```bash
ldd $(pg_config --pkglibdir)/pg_sage.so | grep curl
# Must show: libcurl.so.4 => /usr/lib/x86_64-linux-gnu/libcurl.so.4
# If missing: rebuild with PGXS and ensure SHLIB_LINK includes -lcurl
```

**Edge case: libcurl in background worker.** `curl_global_init()` is NOT thread-safe. It must be called exactly once, at extension load time (`_PG_init()`), NOT in each worker. Each worker uses `curl_easy_init()` per call. Verify this pattern.

**Edge case: DNS resolution timeout.** libcurl's default DNS timeout is separate from `CURLOPT_TIMEOUT`. A bad endpoint can hang on DNS for 5+ minutes. Set:
```c
curl_easy_setopt(curl, CURLOPT_CONNECTTIMEOUT, 10); /* 10s connect timeout */
curl_easy_setopt(curl, CURLOPT_DNS_CACHE_TIMEOUT, 300); /* cache DNS for 5 min */
```

### If LLM code DOES NOT exist:

Document this as a feature gap:

```
The C extension (v0.5.0) does not implement LLM integration.
LLM-enhanced analysis (Tier 2 — cross-query index optimization, INCLUDE column recommendations,
workload-aware index consolidation) is available only in standalone sidecar mode.
Extension mode provides Tier 1 (rules engine) and Tier 3 (autonomous actions) without LLM.
```

**Do NOT implement LLM in C from scratch for this release.** The sidecar already has it working. Adding it to C doubles the maintenance surface (Go HTTP client vs C libcurl, Go JSON parsing vs C cJSON/jansson, Go goroutines vs C background worker SPI). Users who want LLM-enhanced analysis use the sidecar.

Commit: `feat: verify and configure LLM integration via Gemini` or `docs: document LLM as sidecar-only feature`

---

## Fix 5: Auto-Explain End-to-End Path (EXT-FIX-5)

**Priority:** P1
**Not a code fix — a test gap.** The code may be fine; it was just never tested with auto-explain enabled.

### Test procedure

```sql
-- 1. Enable auto-explain
ALTER SYSTEM SET sage.autoexplain_enabled = on;
ALTER SYSTEM SET sage.autoexplain_min_duration_ms = 100;
SELECT pg_reload_conf();

-- 2. Verify settings took effect
SHOW sage.autoexplain_enabled;  -- must show 'on'
SHOW sage.autoexplain_min_duration_ms;  -- must show '100'

-- 3. Run a query that exceeds 100ms
-- Use CROSS JOIN to guarantee slow execution:
SELECT count(*) FROM lineitem l1
CROSS JOIN (SELECT * FROM lineitem LIMIT 10) l2
WHERE l1.l_quantity > 50;
-- Verify this takes > 100ms (it should take 150ms+ on TPC-H data)

-- 4. Wait for collector cycle (sage.collector_interval, default 30s)
SELECT pg_sleep(35);

-- 5. Check explain_cache for captured plan
SELECT queryid, captured_at,
       length(plan_text) AS plan_length,
       left(plan_text, 200) AS plan_preview
FROM sage.explain_cache
WHERE captured_at > now() - interval '2 minutes'
ORDER BY captured_at DESC;
```

**Expected:** At least 1 row with non-NULL `plan_text` containing actual EXPLAIN ANALYZE output (nodes like `Seq Scan`, `Nested Loop`, timing info).

**If explain_cache is empty:**
```sql
-- Check if the table exists at all:
SELECT * FROM information_schema.tables WHERE table_schema = 'sage' AND table_name = 'explain_cache';

-- Check if auto_explain is capturing to log:
-- Look in PG log for: "duration: XXX ms  plan:" entries
-- If log has plans but explain_cache is empty: the hook that transfers
-- from log to table is broken

-- Check if auto_explain extension is loaded:
SELECT * FROM pg_extension WHERE extname = 'auto_explain';
-- pg_sage may use its OWN auto-explain hook, not the standard auto_explain extension.
-- Read the C code to understand the mechanism:
grep -rn "auto_explain\|ExecutorEnd_hook\|ExecutorStart_hook\|explain" src/ --include="*.c"
```

**Edge case: auto-explain and pg_stat_statements interaction.** Both hook into the executor. If pg_sage's auto-explain hook doesn't chain to the previous hook correctly, it can break pg_stat_statements tracking (or vice versa). Verify:
```sql
-- After enabling auto-explain, pg_stat_statements should still work:
SELECT count(*) FROM pg_stat_statements;
-- If this returns 0 or errors: the hooks are conflicting
```

```sql
-- 6. Verify sage.explain(queryid) retrieval
SELECT sage.explain(queryid)
FROM sage.explain_cache
WHERE plan_text IS NOT NULL
LIMIT 1;
-- Must return actual plan text, NOT "No plan available"
```

**If sage.explain() returns "No plan available" even when explain_cache has data:** The function is looking up by queryid but the queryid in explain_cache doesn't match. Check if there's a type mismatch (int32 vs int64) or if the function uses a different lookup key.

Commit: `test: auto-explain end-to-end verification (EXT-FIX-5)` — this commit contains only test scripts and documentation, not code changes (unless bugs are found during testing).

---

## Fix 6: GUC Audit Against Spec (EXT-FIX-7)

**Priority:** P2
**Files:** `src/guc.c`, `docs/pg_sage_spec_v2.2.md`

### Step 1: Dump all GUCs

```sql
SELECT name, setting, unit, short_desc, context, vartype,
       min_val, max_val, enumvals, boot_val, reset_val
FROM pg_settings
WHERE name LIKE 'sage.%'
ORDER BY name;
```

### Step 2: Cross-reference against spec

For EVERY GUC in the spec's GUC Reference section:
- [ ] Exists in `pg_settings` output
- [ ] `setting` (default) matches spec
- [ ] `min_val` / `max_val` match spec
- [ ] `context` matches spec (PGC_SIGHUP, PGC_USERSET, PGC_POSTMASTER, PGC_SUSET)
- [ ] `vartype` matches spec (bool, integer, real, string, enum)
- [ ] `short_desc` is non-empty and accurate

For EVERY GUC in `pg_settings` that is NOT in the spec:
- Document it (undocumented GUC) or remove it (leftover from development)

### Step 3: Verify trust_level check_hook (Bug #2 fix)

```sql
-- Must fail:
ALTER SYSTEM SET sage.trust_level = 'invalid';
ALTER SYSTEM SET sage.trust_level = '';
ALTER SYSTEM SET sage.trust_level = 'OBSERVATION';  -- case sensitivity?

-- Must succeed:
ALTER SYSTEM SET sage.trust_level = 'observation';
ALTER SYSTEM SET sage.trust_level = 'advisory';
ALTER SYSTEM SET sage.trust_level = 'autonomous';
RESET sage.trust_level;
```

**Edge case: case sensitivity.** Does the check_hook accept 'OBSERVATION' (uppercase)? PostgreSQL GUCs are case-insensitive for enum values by convention, but string GUCs with custom check_hooks need explicit `pg_strcasecmp()`. Verify which behavior the check_hook implements and document it.

Commit: `chore: GUC audit against spec, fix any mismatches`

---

## V2 Test Plan — Extension-Specific Mandatory Phases

### Phase 0: Infrastructure (ALL CLUSTERS SIMULTANEOUS)

```bash
# Start all 4 clusters at the same time
for port in 5414 5415 5416 5417; do
    pg_ctl -D /var/lib/postgresql/${port}/data start &
done
wait

# Wait 30s for postmaster + background workers to initialize
sleep 30

# Verify collector is running on ALL 4 BEFORE applying any workload:
for port in 5414 5415 5416 5417; do
    echo "Port $port:"
    psql -p $port -c "SELECT sage.health_json()::jsonb->>'snapshots_total' AS snapshots;"
done
# ALL must show snapshots_total > 0. If any show 0: wait 30s more and recheck.
# Do NOT proceed to workload application until all 4 have snapshots_total > 0.

# THEN apply identical workloads to all 4
for port in 5414 5415 5416 5417; do
    psql -p $port -f workload.sql &
done
wait
```

**Why this matters:** v1 showed PG14 with 12 snapshots vs PG17 with 85. Starting simultaneously and confirming collection before workloads ensures equal observation windows.

### Phase 4b: Auto-Explain Full Path (MANDATORY)

See EXT-FIX-5 above. Run on all 4 versions. All must capture plans.

### Phase 7b: Tier 3 Execution (MANDATORY)

**Before running this phase:** Verify the `sage.config` table structure and `sage.action_log` column names:
```sql
-- Check sage.config table:
\d sage.config
-- Needs a unique constraint on 'key' for ON CONFLICT to work.
-- If no unique constraint: use INSERT ... WHERE NOT EXISTS or DELETE+INSERT instead.

-- Check sage.action_log columns:
\d sage.action_log
-- Adapt the SELECT queries below to match actual column names.
-- The spec names may differ from implementation (e.g., 'action_type' vs 'category', 'outcome' vs 'status')

-- Check if tier3 GUCs exist:
SHOW sage.tier3_safe;
-- If error: tier3 control may be internal C struct, not a GUC. Check:
SELECT name FROM pg_settings WHERE name LIKE 'sage.tier3%';
-- If nothing: Tier 3 is controlled only by trust_level + ramp timing, not per-tier toggles.
```

```sql
-- 1. Backdate trust ramp to 32 days ago (satisfies all tier thresholds)
UPDATE sage.config
SET value = (now() - interval '32 days')::text
WHERE key = 'trust_ramp_start';
-- Verify: must affect 1 row. If 0 rows: INSERT instead:
INSERT INTO sage.config (key, value)
VALUES ('trust_ramp_start', (now() - interval '32 days')::text)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
-- If ON CONFLICT fails ("no unique constraint"): use DELETE + INSERT:
-- DELETE FROM sage.config WHERE key = 'trust_ramp_start';
-- INSERT INTO sage.config (key, value) VALUES ('trust_ramp_start', (now() - interval '32 days')::text);

-- 2. Enable autonomous mode
ALTER SYSTEM SET sage.trust_level = 'autonomous';
SELECT pg_reload_conf();

-- Verify trust level took effect:
SELECT sage.health_json()::jsonb->>'trust_level';
-- Must show 'autonomous'

-- 3. Create conditions for safe actions:

-- 3a. Unused index (for DROP test)
CREATE INDEX idx_test_unused_lineitem ON lineitem (l_comment);
-- Backdate first_seen so it exceeds unused_index_window_days:
-- Try UPDATE first, then INSERT if no row exists:
DO $$
BEGIN
    UPDATE sage.config
    SET value = (now() - interval '60 days')::text
    WHERE key = 'first_seen:public.idx_test_unused_lineitem';

    IF NOT FOUND THEN
        INSERT INTO sage.config (key, value)
        VALUES ('first_seen:public.idx_test_unused_lineitem', (now() - interval '60 days')::text);
    END IF;
END $$;

-- 3b. Bloated table (for VACUUM test)
-- Insert duplicates then delete them to create dead tuples:
WITH to_copy AS (
    SELECT * FROM lineitem WHERE l_linenumber = 1 LIMIT 10000
)
INSERT INTO lineitem SELECT * FROM to_copy;
-- Now delete by identifying the new rows. Since lineitem has a PK, duplicates
-- will fail on INSERT above if there IS a PK on (l_orderkey, l_linenumber).
-- Alternative: use a staging table:
CREATE TEMP TABLE lineitem_bloat AS SELECT * FROM lineitem LIMIT 10000;
INSERT INTO lineitem_bloat SELECT * FROM lineitem_bloat;  -- double it
DELETE FROM lineitem_bloat WHERE ctid > '(100,0)';  -- delete ~half
-- OR simpler: just UPDATE a bunch of rows to create dead versions:
UPDATE lineitem SET l_comment = l_comment || '.' WHERE l_linenumber = 1 LIMIT 10000;
-- NOTE: PG doesn't support LIMIT on UPDATE. Use:
UPDATE lineitem SET l_comment = l_comment || '.'
WHERE ctid IN (SELECT ctid FROM lineitem WHERE l_linenumber = 1 LIMIT 10000);
-- This creates 10000 dead tuple versions. Analyzer should flag bloat.

-- 3c. Idle in transaction (for pg_terminate_backend test)
-- In a SEPARATE psql session (not this one):
-- psql -p 5417 -c "BEGIN; SELECT pg_sleep(999999);"
-- Leave it running in background.
-- Reduce timeout for faster testing:
ALTER SYSTEM SET sage.idle_in_transaction_timeout_minutes = 1;
SELECT pg_reload_conf();
-- Then wait ~2 analyzer cycles for detection + execution.

-- 4. Wait for analyzer + executor cycle
SELECT pg_sleep(900);  -- 15 min

-- 5. Verify actions executed (adapt column names to actual schema):
SELECT * FROM sage.action_log ORDER BY executed_at DESC LIMIT 20;
-- If action_log uses different column names, adjust above.
```

**Expected results:**
- At least 1 `DROP INDEX CONCURRENTLY` action on `idx_test_unused_lineitem` with `outcome = 'success'`
- At least 1 `VACUUM` action with `outcome = 'success'`
- Possibly 1 `pg_terminate_backend` action (if idle session test ran long enough)

**Verify the index was actually dropped:**
```sql
SELECT * FROM pg_indexes WHERE indexname = 'idx_test_unused_lineitem';
-- Must return 0 rows
```

**Verify VACUUM actually ran:**
```sql
SELECT n_dead_tup, last_vacuum FROM pg_stat_user_tables WHERE relname = 'lineitem';
-- n_dead_tup should be significantly lower than before
-- last_vacuum should be recent
```

**Edge case: executor fires but CREATE INDEX CONCURRENTLY fails.** If the executor tries to CREATE an index (from a missing FK finding) and it fails (e.g., lock timeout), the action_log should show `outcome = 'failed'` with error details. Verify the executor doesn't crash or leave an INVALID index behind without logging it.

**Edge case: rollback monitoring.** If an action regresses performance:
1. Create an index on a heavily-queried column
2. Run queries that use it, establishing a baseline
3. The executor drops a different index, causing regression
4. Within `rollback_window_minutes`, the executor should detect regression and execute `rollback_sql`
5. `action_log` should show `outcome = 'rolled_back'` with reason

This is hard to test automatically. At minimum, verify the rollback monitoring goroutine/timer is running by checking `sage.health_json()` for rollback-related fields.

### Phase 12: LLM Integration (CONDITIONAL)

Only run if `SHOW sage.llm_enabled` doesn't error. Otherwise skip with documented reason.

```sql
ALTER SYSTEM SET sage.llm_enabled = on;
ALTER SYSTEM SET sage.llm_endpoint = 'https://generativelanguage.googleapis.com/v1beta/openai/chat/completions';
ALTER SYSTEM SET sage.llm_api_key = '${SAGE_GEMINI_API_KEY}';
ALTER SYSTEM SET sage.llm_model = 'gemini-2.5-flash-preview';
SELECT pg_reload_conf();

-- Wait for 1-2 analyzer cycles
SELECT pg_sleep(1200);

-- Check for LLM-enhanced findings:
SELECT category, severity,
       detail->>'llm_rationale' IS NOT NULL AS has_llm,
       left(detail->>'llm_rationale', 100) AS rationale_preview
FROM sage.findings
WHERE detail->>'llm_rationale' IS NOT NULL
LIMIT 10;

-- Check token usage:
SELECT sage.health_json()::jsonb->'llm' AS llm_status;
-- Expected: {"enabled": true, "calls_total": N, "tokens_used": N, "circuit_state": "closed"}

-- Failure mode: set endpoint to garbage
ALTER SYSTEM SET sage.llm_endpoint = 'https://invalid.example.com/v1/chat/completions';
SELECT pg_reload_conf();

-- Wait for analyzer cycle
SELECT pg_sleep(700);

-- Circuit breaker should be open:
SELECT sage.health_json()::jsonb->'llm'->>'circuit_state';
-- Expected: 'open'

-- Restore endpoint
ALTER SYSTEM SET sage.llm_endpoint = 'https://generativelanguage.googleapis.com/v1beta/openai/chat/completions';
SELECT pg_reload_conf();

-- After cooldown, circuit should close and LLM calls resume
```

### Phase 13: Extension + Sidecar Coexistence (UPGRADED)

Run on PG17 only:

```bash
PG17_DATA="/var/lib/postgresql/5417/data"
PG17_CONF="${PG17_DATA}/postgresql.conf"

# 1. Extension is running, holds advisory lock
psql -p 5417 -c "
    SELECT l.locktype, l.mode, l.granted, a.application_name
    FROM pg_locks l
    JOIN pg_stat_activity a ON l.pid = a.pid
    WHERE l.locktype = 'advisory';"
# Expected: 1 advisory lock, granted=true, application_name contains 'pg_sage'

# 2. Start sidecar in standalone mode against same database
./pg_sage_sidecar --mode=standalone \
    --pg-host=localhost --pg-port=5417 \
    --pg-user=sage_agent --pg-database=sage_test 2>&1 | head -5
# Expected: FATAL "Another pg_sage instance holds the advisory lock on this database." Exit 1.

# 3. Stop PG
pg_ctl -D ${PG17_DATA} stop -m fast

# 4. Remove pg_sage from shared_preload_libraries and restart WITHOUT extension
# Save original config:
cp ${PG17_CONF} ${PG17_CONF}.bak
# Comment out pg_sage (keep pg_stat_statements):
sed -i "s/shared_preload_libraries = 'pg_stat_statements,pg_sage'/shared_preload_libraries = 'pg_stat_statements'/" ${PG17_CONF}
# If the line format is different, use a more flexible pattern:
# sed -i "s/,pg_sage//" ${PG17_CONF}

pg_ctl -D ${PG17_DATA} start
sleep 5

# 5. Start sidecar → should succeed, create schema, take lock
./pg_sage_sidecar --mode=standalone \
    --pg-host=localhost --pg-port=5417 \
    --pg-user=sage_agent --pg-database=sage_test &
SIDECAR_PID=$!
sleep 15

# 6. Verify sidecar is running and has taken advisory lock
psql -p 5417 -c "SELECT * FROM sage.config WHERE key = 'trust_ramp_start';"
# Should return a row (sidecar initialized or reused the schema)
psql -p 5417 -c "SELECT locktype, mode, granted FROM pg_locks WHERE locktype = 'advisory';"
# Should show advisory lock held

# 7. Stop sidecar
kill $SIDECAR_PID
wait $SIDECAR_PID 2>/dev/null
sleep 5

# 8. Restore extension config and restart PG with extension
cp ${PG17_CONF}.bak ${PG17_CONF}
pg_ctl -D ${PG17_DATA} restart
sleep 10

# 9. Verify extension reclaimed control
psql -p 5417 -c "SELECT sage.health_json()::jsonb->>'workers_active' AS workers;"
# Expected: 3 workers running

# 10. Cleanup
rm -f ${PG17_CONF}.bak
```

**Edge case: schema collision.** Both extension and sidecar create the `sage` schema. If the sidecar's schema DDL differs from the extension's (e.g., different column names, extra tables), the extension may fail to start because its SPI queries reference columns that don't exist in the sidecar's schema version. Verify both use the same schema DDL, or that the extension handles schema migration gracefully.

### Phase 14: Cross-Version Comparison

After all phases complete (with simultaneous start):

```sql
-- Run on each port, compare results:
SELECT
    current_setting('server_version') AS pg_version,
    (SELECT count(*) FROM sage.snapshots) AS snapshot_count,
    (SELECT count(*) FROM sage.findings) AS finding_count,
    (SELECT count(*) FROM sage.findings WHERE category = 'schema_design') AS schema_design_count,
    (SELECT count(*) FROM sage.findings WHERE category = 'unused_index') AS unused_index_count,
    (SELECT count(*) FROM sage.findings WHERE category = 'slow_query') AS slow_query_count,
    (SELECT count(*) FROM sage.action_log) AS actions_count;
```

**Acceptance criteria:**
- Snapshot counts within ±10% across all 4 versions
- Finding counts within ±5% across PG14/15/16 (PG17 may have slightly more due to `pg_stat_checkpointer` additional data)
- Slow query counts should be identical (same workload, same queries)
- Action counts should be identical (same trust ramp backdating, same conditions)

---

## Build & Deploy

```bash
# Build for all 4 versions
for ver in 14 15 16 17; do
    PG_CONFIG=/usr/lib/postgresql/${ver}/bin/pg_config make clean
    PG_CONFIG=/usr/lib/postgresql/${ver}/bin/pg_config make
    PG_CONFIG=/usr/lib/postgresql/${ver}/bin/pg_config make install
done

# Verify zero compiler warnings
# If any warnings: fix them. Warnings in C extension code are potential crashes.

# Restart all clusters to pick up new shared library
for port in 5414 5415 5416 5417; do
    pg_ctl -D /var/lib/postgresql/${port}/data restart
done

# Verify version on each
for port in 5414 5415 5416 5417; do
    psql -p $port -c "SELECT sage.health_json()::jsonb->>'version' AS version;"
done
# All must show 0.6.0

# Tag
cd pg_sage
git tag v0.6.0-rc1
git push origin fix/ext-v0.6
git push origin v0.6.0-rc1
```

---

## Definition of Done

### Bug fixes
- [ ] SPI error handling: rapid DROP/CREATE cycling (10x) produces zero ERROR log entries — only LOG
- [ ] Schema exclusion: zero sage.* objects in findings (except sage_health category)
- [ ] Schema exclusion: defense-in-depth filter in analyzer rule loop
- [ ] trust_level check_hook: rejects 'invalid', accepts 'observation'/'advisory'/'autonomous'
- [ ] trust_level check_hook: case sensitivity behavior documented

### Noise reduction
- [ ] TOAST bloat: tables below `toast_bloat_min_rows` (default 1000) excluded
- [ ] Schema design: tables below `schema_design_min_rows` (default 100) excluded
- [ ] Schema design: tables below `schema_design_min_columns` (default 2) excluded
- [ ] TPC-H schema alone (no edge cases): fewer than 50 total findings

### Auto-explain
- [ ] `sage.autoexplain_enabled = on` → slow queries captured in `sage.explain_cache`
- [ ] Captured plans contain real EXPLAIN ANALYZE output (nodes, timing)
- [ ] `sage.explain(queryid)` returns plan text, not "No plan available"
- [ ] Auto-explain doesn't break pg_stat_statements (both hooks coexist)

### Tier 3 execution
- [ ] `sage.config` table structure verified (has `key` column with unique constraint, or workaround used)
- [ ] `sage.action_log` column names verified against actual DDL (may differ from spec)
- [ ] With backdated trust ramp: DROP INDEX CONCURRENTLY executed on unused index
- [ ] With backdated trust ramp: VACUUM executed on bloated table
- [ ] Actions logged in sage.action_log with success outcome and before/after state
- [ ] Dropped index confirmed missing from pg_indexes
- [ ] VACUUM confirmed via reduced n_dead_tup in pg_stat_user_tables
- [ ] Emergency stop: `UPDATE sage.config SET value='true' WHERE key='emergency_stop'` halts executor

### LLM integration
- [ ] If implemented: Gemini connected, LLM-enhanced findings generated, token tracking works
- [ ] If implemented: API key not visible to non-superuser (file-based or PGC_SUSET)
- [ ] If implemented: circuit breaker opens on bad endpoint, closes after cooldown
- [ ] If implemented: `curl_global_init()` called once in `_PG_init()`, not per-worker
- [ ] If implemented: libcurl linked (`ldd pg_sage.so | grep curl`)
- [ ] If not implemented: documented as sidecar-only, no LLM GUCs exist

### Cross-version parity
- [ ] All 4 clusters started simultaneously, collector confirmed running before workloads
- [ ] Snapshot counts within ±10% across all 4 versions
- [ ] Finding counts within ±5% across PG14-17 (excluding version-specific features)
- [ ] Slow query counts identical across all versions

### Extension + sidecar coexistence
- [ ] Sidecar blocked by extension's advisory lock (exits with FATAL)
- [ ] Sidecar takes over after extension stops
- [ ] Extension reclaims control after sidecar stops
- [ ] Schema compatible between extension and sidecar modes

### GUC audit
- [ ] Every spec GUC exists with correct default, range, and context
- [ ] No undocumented GUCs (or newly documented)
- [ ] All new GUCs (toast_bloat_min_rows, schema_design_*) registered and documented

### Commits and build
- [ ] Each fix is an atomic commit with descriptive message
- [ ] Tagged v0.6.0-rc1
- [ ] Builds clean on PG14/15/16/17 with ZERO compiler warnings
- [ ] All 4 clusters restart clean with new extension version
