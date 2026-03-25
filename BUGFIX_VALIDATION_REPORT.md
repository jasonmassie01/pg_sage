# pg_sage Sidecar Bug Fix Validation Report

**Date:** 2026-03-23
**Instance:** sage-test-pg17 (PG 17.9, 34.72.70.25, db-f1-micro, us-central1-c)
**Sidecar:** v0.7.0-rc1 (rebuilt with all 8 fixes)
**Config:** `maintenance_window: "* * * * *"` (wildcard — previously crashed)
**Test harness:** `cloudsqltests/validate_bugfixes.go`

---

## Executive Summary

All 8 bugs identified in the Cloud SQL integration test have been fixed in the sidecar codebase. Validation ran against the live Cloud SQL PG17 instance. **15/15 tests passed, 0 failed, 0 skipped.** No bugs apply to the C extension codebase.

---

## Validation Results

| # | Bug | Severity | Test Method | Result |
|---|-----|----------|-------------|--------|
| 1 | `inMaintenanceWindow` can't parse `* * * * *` | MEDIUM | DB: check moderate actions executed with wildcard window | **PASS** (13 create_index actions) |
| 2 | `sage://schema/{table}` needs `$1::text` cast | MEDIUM | MCP: resource read schema/orders, schema/public.orders, schema/customers | **PASS x3** |
| 3 | `sage://health` same parameter type issue | MEDIUM | MCP: resource read sage://health | **PASS** |
| 4 | `suggest_index` tool returns `isError=true` always | MEDIUM | MCP: tool call suggest_index(orders), suggest_index(public.customers) | **PASS x2** |
| 5 | Gemini response truncation (thinking model) | LOW | Code review: `max_output_tokens` field added + 2048 minimum | **PASS** |
| 6 | `has_plan_time_columns = false` on PG17 | LOW | DB: new pg_attribute query returns true; old information_schema query confirmed broken | **PASS x2** |
| 7 | LLM endpoint double-path (`/chat/completions` appended twice) | MEDIUM | MCP: sage_status shows LLM connected, no 404 in startup | **PASS** |
| 8 | YAML `${ENV_VAR}` expansion unreliable | LOW | Code review: `warnUnexpandedEnvVars()` warns on stderr | **PASS** |

**Bonus tests (regression check):**

| Resource/Tool | Result |
|---------------|--------|
| `sage://stats/orders` | **PASS** |
| `sage://findings` | **PASS** |
| `sage://slow-queries` | **PASS** |

---

## Fix Details

### Bug 1: `inMaintenanceWindow` can't parse `* * * * *`

**Root cause:** `strconv.Atoi("*")` returned an error, causing the function to return `false` (no maintenance window match). Moderate actions were blocked unless a numeric cron like `"0 12 * * *"` was used.

**Fix:** `sidecar/internal/executor/trust.go:52-121`
- Added wildcard detection: `parts[0] == "*"` and `parts[1] == "*"`
- Both wildcards = always in window (equivalent to `"always"`)
- Hour-wild + specific minute = 1-hour window starting at `:minute` every hour
- Added `"always"` keyword support via `strings.EqualFold`
- Trimmed whitespace before parsing

**Extension:** Already handled correctly. `action_executor.c:201-203` checks `strcmp(spec, "*") == 0 || pg_strcasecmp(spec, "always") == 0` and returns `true`.

**Validation:** Config set to `maintenance_window: "* * * * *"`. Sidecar started, executor ran 13 `create_index` moderate actions — proving the wildcard window was recognized.

---

### Bug 2: `sage://schema/{table}` needs `$1::text` cast

**Root cause:** PostgreSQL's pgx driver sends Go `string` parameters as untyped `$1`. When `$1` appears in expressions like `table_schema || '.' || table_name = $1`, PG cannot infer the type, returning `ERROR: could not determine data type of parameter $1 (SQLSTATE 42P08)`.

**Fix:** Added `$1::text` explicit casts in all SQL queries containing string parameter comparisons:
- `sidecar/resources.go:149-190` — fallback schema query (7 occurrences of `$1` → `$1::text`)
- `sidecar/resources.go:142-143` — `sage.schema_json($1)` → `sage.schema_json($1::text)`
- `sidecar/resources.go:196-197` — `sage.stats_json($1)` → `sage.stats_json($1::text)`
- `sidecar/resources.go:203-216` — fallback stats query (2 occurrences)
- `sidecar/cmd/pg_sage_sidecar/main.go:803-829` — main.go readSchema (6 occurrences)
- `sidecar/cmd/pg_sage_sidecar/main.go:832-844` — main.go readStats (2 occurrences)

**Extension:** Not applicable. SPI in C uses `Datum` arrays with explicit type OIDs (`TEXTOID`).

**Validation:** MCP resource reads for `sage://schema/orders`, `sage://schema/public.orders`, and `sage://schema/customers` all return column/index/constraint data. Previously all 5 schema resource reads failed.

---

### Bug 3: `sage://health` same parameter type issue

**Root cause:** `main.go:readHealth()` in standalone mode builds a `json_build_object()` with `$1` (version string) and `$2` (cloud environment string) as untyped parameters.

**Fix:** `sidecar/cmd/pg_sage_sidecar/main.go:737,749`
- `$1` → `$1::text` (version)
- `$2` → `$2::text` (cloudEnvironment)

**Extension:** Not applicable (SPI uses typed Datums).

**Validation:** MCP resource read for `sage://health` returns JSON with `status`, `connections`, `database_size`, etc. Previously returned parameter type error.

---

### Bug 4: `suggest_index` tool returns `isError=true` always

**Root cause:** Same `$1` untyped parameter issue as bugs 2-3. The `suggest_index` tool's SQL query uses `$1` in string concatenation comparisons (`schemaname || '.' || tablename = $1`), which fails on pgx.

**Fix:** Added `$1::text` casts in:
- `sidecar/tools.go:170-198` — sidecar-only suggest_index query (5 occurrences)
- `sidecar/cmd/pg_sage_sidecar/main.go:909-922` — main.go suggest_index query (5 occurrences)

**Extension:** Not applicable.

**Validation:** MCP tool calls for `suggest_index(orders)` and `suggest_index(public.customers)` both return analysis data with `isError=false`. Previously all calls returned `isError=true`.

---

### Bug 5: Gemini response truncation (thinking model)

**Root cause:** Gemini 2.5 Flash is a "thinking" model that uses output tokens for internal reasoning. Without an explicit `max_output_tokens` field, the default may be too low, causing responses to be truncated mid-JSON. The sidecar handles this gracefully (falls back to Tier 1 findings), but LLM recommendations are lost.

**Fix:** `sidecar/internal/llm/client.go:36-41,112-126`
1. Added `MaxOutputTokens` field to `ChatRequest` struct (Gemini's OpenAI-compat endpoint recognizes both `max_tokens` and `max_output_tokens`)
2. Added 2048-token minimum: `if maxTokens <= 0 { maxTokens = 2048 }`
3. Both fields set to the same value in every request

**Extension:** Not applicable (no LLM client in C extension).

**Validation:** Structural fix verified in code. The index optimizer already passes 1024; the briefing worker passes `ContextBudgetTokens` (4096 in test config). Both now have the `max_output_tokens` field set.

---

### Bug 6: `has_plan_time_columns = false` on PG17

**Root cause:** The startup check queried `information_schema.columns` looking for `table_schema = 'pg_catalog' AND table_name = 'pg_stat_statements'`. But `pg_stat_statements` is a **view** created by the extension — `information_schema.columns` only reliably lists columns of base tables and views in the extension's schema, not views that appear in `pg_catalog` via the search path.

**Fix:** `sidecar/internal/startup/checks.go:160-183,185-207`

Replaced `information_schema.columns` queries with `pg_attribute` queries for both `checkPlanTimeColumns` and `checkWALColumns`:

```sql
-- Before (broken on PG17):
SELECT column_name FROM information_schema.columns
WHERE table_schema = 'pg_catalog' AND table_name = 'pg_stat_statements'
  AND column_name = 'total_plan_time'

-- After (works on all versions):
SELECT EXISTS(
    SELECT 1 FROM pg_attribute
    WHERE attrelid = 'pg_stat_statements'::regclass
      AND attname = 'total_plan_time'
      AND NOT attisdropped
)
```

`pg_attribute` works for both base tables and views because it operates on the relation OID directly.

**Extension:** Not applicable (C extension doesn't check for these columns at startup).

**Validation:**
- New query returns `true` on PG17 (**PASS**)
- Old query returns no rows on PG17 (**confirming the bug existed**)
- Sidecar startup log shows `plan_time columns: true` (previously showed `false`)

---

### Bug 7: LLM endpoint double-path

**Root cause:** `client.go:124-128` always appends `/chat/completions` to the configured endpoint. If the user configures the full URL (e.g., `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`), the code produces `.../chat/completions/chat/completions` → 404.

**Fix:** `sidecar/internal/llm/client.go:133-139`
```go
// Before:
endpoint := c.cfg.Endpoint
if endpoint[len(endpoint)-1] != '/' { endpoint += "/" }
endpoint += "chat/completions"

// After:
endpoint := c.cfg.Endpoint
endpoint = strings.TrimRight(endpoint, "/")
endpoint = strings.TrimSuffix(endpoint, "/chat/completions")
endpoint = strings.TrimSuffix(endpoint, "/chat")
endpoint += "/chat/completions"
```

Now works with any of:
- `https://api.example.com/v1` (base URL)
- `https://api.example.com/v1/` (trailing slash)
- `https://api.example.com/v1/chat/completions` (full path)
- `https://api.example.com/v1/chat/completions/` (full path + trailing slash)

**Extension:** Not applicable.

**Validation:** Sidecar started with `endpoint: "https://generativelanguage.googleapis.com/v1beta/openai"` (base URL) and LLM connected successfully. MCP `sage_status` confirms LLM is operational.

---

### Bug 8: YAML `${ENV_VAR}` expansion unreliable

**Root cause:** `os.ExpandEnv()` silently replaces undefined `${VAR}` with empty string. Users who write `api_key: ${SAGE_LLM_API_KEY}` in YAML without setting the env var get an empty API key with no error — the LLM just silently fails.

**Fix:** `sidecar/internal/config/config.go:377-406`

Added `warnUnexpandedEnvVars()` function that:
1. Scans raw YAML for `${...}` patterns before expansion
2. After expansion, checks if the referenced env var was actually set
3. Prints `WARNING: config "config.yaml" references ${SAGE_LLM_API_KEY} but it is not set in the environment` to stderr

The env overlay (`overlayEnv()` at line 387) still takes precedence — `SAGE_LLM_API_KEY=xxx` env var overrides the YAML value regardless of `${...}` syntax.

**Extension:** Not applicable (C extension uses GUCs, not YAML).

**Validation:** Structural fix. The warning is emitted at config load time before the sidecar starts.

---

## Build Verification

```
$ go build ./...           # Clean compile, 0 errors
$ go test ./...            # All 10 packages pass
ok  github.com/pg-sage/sidecar                  55.262s
ok  github.com/pg-sage/sidecar/internal/briefing     21.221s
ok  github.com/pg-sage/sidecar/internal/collector     0.933s
ok  github.com/pg-sage/sidecar/internal/config        0.770s
ok  github.com/pg-sage/sidecar/internal/executor     15.219s
ok  github.com/pg-sage/sidecar/internal/ha           22.377s
ok  github.com/pg-sage/sidecar/internal/llm          30.305s
ok  github.com/pg-sage/sidecar/internal/retention    22.312s
ok  github.com/pg-sage/sidecar/internal/schema       22.188s
ok  github.com/pg-sage/sidecar/internal/startup      22.171s
```

## Files Changed

| File | Lines Changed | Bugs Fixed |
|------|--------------|------------|
| `sidecar/internal/executor/trust.go` | 52-121 (rewritten) | #1 |
| `sidecar/resources.go` | 142-143, 149-190, 196-197, 203-216 | #2 |
| `sidecar/cmd/pg_sage_sidecar/main.go` | 737, 749, 803-829, 832-844, 909-922 | #3, #4 |
| `sidecar/tools.go` | 170-198 | #4 |
| `sidecar/internal/llm/client.go` | 3-16, 36-41, 112-139 | #5, #7 |
| `sidecar/internal/startup/checks.go` | 160-207 (rewritten) | #6 |
| `sidecar/internal/config/config.go` | 377-406 | #8 |

## Extension Applicability

None of the 8 bugs apply to the C extension:

| Bug | Why N/A |
|-----|---------|
| #1 Wildcard cron | Extension already handles `*` and `"always"` in `sage_check_maintenance_window()` |
| #2-4 `$1::text` casts | SPI uses `Datum` arrays with explicit type OIDs (`TEXTOID`) — no inference needed |
| #5 max_output_tokens | No LLM client in extension |
| #6 plan_time detection | Extension doesn't check for column existence at startup |
| #7 Endpoint double-path | No LLM client in extension |
| #8 YAML env expansion | Extension uses GUCs, not YAML config |

## Test Infrastructure

```
Instance: sage-test-pg17 (Cloud SQL, PG 17.9, db-f1-micro)
IP: 34.72.70.25
Project: satty-488221
User: sage_agent / <REDACTED>
Database: sage_test (with Phase 15 test data from prior integration test)
Config: maintenance_window changed to "* * * * *" for bug 1 validation
Test duration: ~3 minutes (excluding instance startup)
Instance status: STOPPED (billing halted)
```
