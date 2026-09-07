# Spec Review: v0.10 Schema Intelligence

**Reviewer**: Claude (spec review)
**Date**: 2026-04-13
**Spec**: `specs/v0.10-schema-intelligence.md`
**Verdict**: Ambitious and well-structured, but 40+ gaps spanning ambiguity,
missing edge cases, contradictions, under-specified interfaces, dependency
risks, and version-specific behavior. Many are P0 blockers that would cause
two engineers to implement fundamentally different things.

---

## 1. Ambiguity (Under-Specified Behavior)

### 1.1 DDL detection requires `log_statement = 'ddl'` but v0.9.1 logwatch requires `log_destination` includes jsonlog/csvlog

The spec says DDL detection parses `log_statement = 'ddl'` entries from
PostgreSQL logs via the logwatch infrastructure. But `log_statement` controls
WHAT is logged, while `log_destination`/`logging_collector` control WHERE and
HOW it is logged. The spec conflates these two settings.

> "Parse `log_statement = 'ddl'` entries from PostgreSQL logs. The logwatch
> parser already handles structured log lines; add a new signal category for
> DDL statements."

Two engineers would disagree on: does this feature require BOTH
`log_statement >= 'ddl'` AND `log_destination` includes csvlog/jsonlog? Or
can it work with plain `stderr` logs? The existing logwatch only supports
csvlog and jsonlog.

**Amendment**: Add a prerequisites section: "DDL log detection requires: (1)
`log_statement` set to `'ddl'` or `'all'`, AND (2) the v0.9.1 logwatch
infrastructure is active (which itself requires csvlog or jsonlog). If
logwatch is disabled, DDL detection falls back to `activity_polling` mode
only. If `log_statement` is `'none'` or `'mod'`, DDL statements other than
DML are not logged; emit a startup warning: 'log_statement is not set to ddl
or all; DDL log detection disabled, using activity polling only.'"

### 1.2 "table > 10K rows" threshold is used by DDL rules (Section 1.4) AND schema lint rules (Section 2.2) but with DIFFERENT config keys

> Section 1.4: `migration.small_table_threshold: 10000`
> Section 2.2: `schema_lint.rules.small_table_threshold: 1000`

These are different thresholds for conceptually similar decisions. A table
with 5,000 rows is "small" for DDL safety but "large" for schema lint. Two
engineers would argue about whether to unify these or keep them separate, and
code that accidentally references the wrong one would silently produce wrong
results.

**Amendment**: Keep them separate (they serve different purposes -- DDL lock
safety vs. finding noise suppression) but rename for clarity:
`migration.ddl_row_threshold: 10000` and
`schema_lint.rules.min_table_rows: 1000`. Document the distinction
explicitly: "DDL thresholds determine when a DDL operation is considered
dangerous enough to warrant advisory output. Lint thresholds determine when
a table is large enough to make a finding actionable."

### 1.3 `ddl_add_column_volatile_default` -- what constitutes "volatile"?

> `ADD COLUMN ... DEFAULT <volatile>` -- ACCESS EXCLUSIVE (rewrite)

The spec does not define what "volatile" means in this context. In PostgreSQL,
a function's volatility classification (VOLATILE, STABLE, IMMUTABLE) is
stored in `pg_proc.provolatile`. But the DDL classifier parses SQL strings
from log lines -- it does not have the parsed expression tree or catalog
access to the function's volatility at parse time.

Additionally, this rule is version-dependent. Before PG11, ALL defaults on
ADD COLUMN caused a table rewrite. PG11+ only rewrites for volatile defaults.
The spec targets PG12+ but does not acknowledge this history.

**Amendment**: Define "volatile default" for the classifier:
1. Any function call except `now()`, `current_timestamp`,
   `clock_timestamp()`, `gen_random_uuid()`, `uuid_generate_v4()` --
   maintain an explicit allowlist of known-immutable/stable defaults.
2. Any subquery.
3. `nextval()` calls.
Alternatively, query `pg_proc.provolatile` for function references in the
DEFAULT expression. Document that PG11+ treats immutable/stable defaults as
metadata-only operations, and pg_sage relies on this PG11+ behavior.

### 1.4 DDL classifier input: raw SQL string parsing is fragile

The entire DDL classification system (Section 1.4) relies on parsing SQL
strings from log lines using pattern matching:

> `statement: ALTER TABLE ...`
> `statement: CREATE INDEX ...`

The spec does not define a SQL parser strategy. Two engineers would take
completely different approaches:
- Regex-based pattern matching (fast, fragile, misses edge cases like
  multi-line statements, comments, quoted identifiers)
- Full SQL parser (robust, heavy dependency, complex)

Consider: `ALTER TABLE "CREATE INDEX" ADD COLUMN x int;` -- the table name
contains "CREATE INDEX" as a quoted identifier. A regex matcher would
misclassify this.

**Amendment**: Specify the parsing approach. Recommended: use a lightweight
keyword-based classifier that tokenizes the SQL string (respecting quoted
identifiers and comments), then matches on the first N tokens. Do NOT use
regex on raw SQL. Define the tokenization rules explicitly:
1. Split on whitespace
2. Respect double-quoted identifiers (`"..."`)
3. Respect string literals (`'...'`)
4. Respect block comments (`/* ... */`) and line comments (`-- ...`)
5. Case-insensitive token matching

### 1.5 Risk score formula produces 0.0 for all metadata-only operations

The risk score formula:

> `risk_score = base_risk * (0.4 * table_factor + ...)`
> `rewrite_weight` for "Metadata only": 0.2

For a metadata-only `ALTER TABLE` on a 1-row table with 0 active queries,
0 pending locks, and 0 replication lag: `base_risk = 1.0 * 0.2 = 0.2`,
`table_factor = log10(1) / 10 = 0.0`, all other factors = 0.0.
`risk_score = 0.2 * 0.0 = 0.0`.

This means metadata-only operations ALWAYS produce risk_score = 0.0 on idle
small tables, even ACCESS EXCLUSIVE ones. The `ddl_missing_lock_timeout` rule
fires for any ACCESS EXCLUSIVE DDL, but if risk_score < 0.3 (the incident
threshold), no incident is generated.

**Amendment**: The formula needs a floor. Replace the multiplicative form with
an additive one:
```
risk_score = base_risk * max(0.1,
    0.4 * table_factor
  + 0.3 * activity_factor
  + 0.2 * lock_queue_factor
  + 0.1 * repl_factor)
```
Or add a minimum risk_score for ACCESS EXCLUSIVE operations: `risk_score =
max(risk_score, 0.1)` when lock_level is ACCESS EXCLUSIVE. This ensures
ACCESS EXCLUSIVE DDL always appears in the output even on small idle tables.

### 1.6 Schema lint `Finding` struct vs. existing analyzer `Finding` struct

The spec defines a new `Finding` type in `internal/schema/lint/`:

> ```go
> type Finding struct {
>     RuleID, Schema, Table, Column, Index, Severity, Category,
>     Description, Impact, TableSize, QueryCount, Suggestion, SQL string
> }
> ```

The existing codebase already has `analyzer.Finding`:

```go
type Finding struct {
    Category, Severity, ObjectType, ObjectIdentifier, Title string
    Detail map[string]any
    Recommendation, RecommendedSQL, RollbackSQL, ActionRisk string
}
```

Two engineers would disagree: should schema lint findings use the existing
`analyzer.Finding` type (reuse) or the new type (cleaner separation)?

**Amendment**: Define the relationship explicitly. Recommended: schema lint
uses its own `lint.Finding` type internally (for persistence to
`sage.schema_findings`), but surfaces results as `analyzer.Finding` objects
when feeding into the incident pipeline. Add a `ToAnalyzerFinding()` method
on `lint.Finding`. This keeps the lint package self-contained while
integrating with the existing incident flow.

### 1.7 N+1 detection: "calls that divides evenly" is imprecise

> "Has a call count that divides evenly into the candidate's call count"

What tolerance? If parent has 100 calls and child has 1,003 calls, is the
ratio "~10" close enough? What about 100 parent calls and 987 child calls?
"Divides evenly" implies exact division, which almost never happens in
production due to stats reset timing, concurrent requests, and sampling.

**Amendment**: Replace "divides evenly" with a tolerance-based ratio check:
```
ratio = child_calls / parent_calls
is_correlated = (ratio > 2.0) AND (ratio - floor(ratio)) / ratio < 0.1
```
This accepts ratios within 10% of an integer. Document the tolerance and make
it configurable: `n_plus_one.ratio_tolerance: 0.1`.

### 1.8 Matview staleness: `last_refresh` is "tracked by pg_sage" but how?

> `LastRefresh *time.Time // tracked by pg_sage`

PostgreSQL does not expose matview last refresh time in any catalog. The spec
says pg_sage tracks it, but never defines the tracking mechanism. Options:
- Observe `REFRESH` statements in logs (requires logwatch + log_statement)
- Store refresh times in a `sage.matview_refreshes` table
- Use `pg_stat_user_tables.last_analyze` as a proxy (unreliable)

**Amendment**: Add a `sage.matview_refresh_log` table:
```sql
CREATE TABLE IF NOT EXISTS sage.matview_refresh_log (
    matview_schema TEXT NOT NULL,
    matview_name   TEXT NOT NULL,
    refreshed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    duration_ms    BIGINT,
    concurrent     BOOLEAN,
    PRIMARY KEY (matview_schema, matview_name)
);
```
Update this table: (a) after any pg_sage-initiated refresh, and (b) when a
`REFRESH MATERIALIZED VIEW` statement is detected in logs. For matviews
refreshed externally without logging, staleness cannot be tracked -- document
this limitation. On first discovery, `LastRefresh` is NULL (unknown).

---

## 2. Missing Edge Cases

### 2.1 pg_stat_statements not enabled

N+1 detection (Section 3) and impact correlation (Section 2.4) both depend
on `pg_stat_statements`. The spec never addresses the case where the
extension is not installed or not loaded.

**Amendment**: Add startup detection. If `pg_stat_statements` is not in
`shared_preload_libraries`, disable N+1 detection and impact correlation
with a warning: "pg_stat_statements not loaded; N+1 detection and schema
lint impact correlation disabled." The `lint_jsonb_in_joins` rule (which
cross-references JSONB columns with query patterns) must also be disabled.
Document which features require pg_stat_statements vs. which are catalog-only.

### 2.2 Standby/replica databases

On a standby, DDL cannot execute. The DDL Safety Advisor would never fire
via log detection (standbys don't execute DDL). But the schema linter
SHOULD still run -- standbys have the same schema anti-patterns.

However, `pg_stat_statements` on a standby reflects only read queries
routed to it, not the full workload. N+1 detection on a standby would only
see read patterns, missing write-path N+1 issues.

The matview refresher cannot execute `REFRESH` on a standby.

**Amendment**: Add a section: "Standby Behavior." On standby databases:
- Schema lint: runs normally (read-only catalog queries)
- DDL advisor: disabled (no DDL on standbys)
- N+1 detection: runs but add a note to findings: "Detected on standby;
  call counts reflect read-replica traffic only"
- Matview refresh: disabled (read-only); staleness detection still runs
  but cannot remediate
Detect standby status via `pg_is_in_recovery()`.

### 2.3 Partitioned tables

The spec does not mention partitioned tables anywhere. This creates multiple
ambiguities:

1. **DDL rules**: `ALTER TABLE parent_table ADD COLUMN ...` on a partitioned
   table propagates to all children. The lock behavior differs -- PG locks
   the parent and each child sequentially. The risk assessor's `TableSizeBytes`
   should reflect the TOTAL size across all partitions, not just the parent
   (which has 0 rows).

2. **Schema lint**: Does `lint_missing_fk_index` check the parent, the
   children, or both? FKs on partitioned tables are inherited by children
   in PG12+, but indexes are NOT automatically created on children.

3. **Sequence monitor**: Partitioned tables with identity columns share one
   sequence across all partitions. The insertion rate calculation should
   sum `n_tup_ins` across all children.

4. **N+1 detection**: Queries against partitioned tables appear in
   `pg_stat_statements` referencing the parent table name. The `reltuples`
   for the parent may be 0 while children hold millions of rows.

**Amendment**: Add a "Partitioned Tables" subsection to each feature:
- DDL advisor: compute risk using `pg_total_relation_size()` which includes
  partitions. For `reltuples`, sum across `pg_class` for all `pg_inherits`
  children.
- Schema lint: check children independently for missing indexes. Flag if a
  FK index exists on the parent but not on a child partition.
- Sequence monitor: already handles this (sequences are standalone objects).
- N+1: use partition-aware row counts.

### 2.4 Temporary tables

Temporary tables appear in `pg_class` within `pg_temp_N` schemas. The schema
linter could flag missing PK on temp tables, which is pure noise.

**Amendment**: Exclude schemas matching `pg_temp_%` and `pg_toast_temp_%`
from all lint rules and DDL classification. Add this filter to the base
query or the Linter's schema exclusion list.

### 2.5 Non-public schemas

The matview discovery query filters `WHERE schemaname NOT IN ('pg_catalog',
'information_schema')` -- good. But the schema lint rules and DDL advisor
don't mention schema filtering at all.

The `ForeignKey` struct in the existing collector has no `SchemaName` field:
```go
type ForeignKey struct {
    TableName, ReferencedTable, FKColumn, ConstraintName string
}
```

This means `lint_missing_fk_index` cannot distinguish between
`public.orders` and `analytics.orders`. The rule would produce incorrect
findings when tables with the same name exist in different schemas.

**Amendment**:
1. All lint rules must be schema-aware. The Rule's `Check()` method should
   query with explicit schema joins.
2. The `lint.Finding` struct already has a `Schema` field -- good.
3. The `ForeignKey` collector type needs `SchemaName` and
   `ReferencedSchema` fields (this is a pre-existing gap, not new to v0.10,
   but v0.10 exposes it).
4. Add a `schema_lint.include_schemas` config list. Default: all non-system
   schemas. Allow users to restrict to specific schemas.

### 2.6 RDS/Aurora/Cloud SQL restrictions

Managed PostgreSQL services restrict several operations the spec assumes:

1. **File system access**: RDS/Aurora/Cloud SQL do not expose the PostgreSQL
   log directory to users. The logwatch-based DDL detection will not work.
   The spec's fallback (activity polling) is the only option.

2. **Extension availability**: `pg_stat_statements` may not be installed
   by default on all managed services (it is on RDS, but may require manual
   enablement on Cloud SQL).

3. **Superuser privileges**: The `pg_sequences` view requires SELECT
   privilege. On RDS, the `rds_superuser` role has this, but custom roles
   may not.

4. **DDL restrictions**: Some managed services restrict certain DDL (e.g.,
   `CREATE EXTENSION`, `ALTER SYSTEM`). The DDL classifier should not
   flag operations that the user cannot execute anyway.

5. **`pg_repack`**: The `ddl_vacuum_full` rule recommends `pg_repack` as an
   alternative, but this extension is not available on all managed services.

**Amendment**: Add a "Managed Service Compatibility" section. Specify:
- Log-based DDL detection: "Not available on managed services that do not
  expose log files. Falls back to activity polling."
- For recommendations, add a `managed_service` config option
  (`rds | aurora | cloudsql | alloydb | none`). When set, filter out
  recommendations that reference unavailable features (`pg_repack`,
  `ALTER SYSTEM`, etc.).
- Alternative: detect the platform automatically by querying
  `SHOW rds.extensions` (RDS) or checking for
  `cloudsql.enable_pgaudit` (Cloud SQL).

### 2.7 pg_stat_statements query text truncation

`pg_stat_statements` truncates query text at `track_activity_query_size`
(default: 1024 bytes). Long DDL statements, complex N+1 candidate queries,
and matview definitions may be truncated.

For N+1 detection, a truncated query like `SELECT * FROM orders WHERE id =
$1 AND customer_id = $2 AND status IN ($3, $4, $5, ...` cannot be reliably
classified. The regex `query ~* 'SELECT.*FROM.*WHERE.*=\s*\$'` may not match
if the `WHERE` clause is truncated before the `= $` part.

**Amendment**: Document the limitation: "N+1 detection accuracy degrades for
queries longer than `track_activity_query_size`. Recommend setting
`track_activity_query_size = 4096` or higher for best results." Add a
startup check: if `track_activity_query_size < 2048`, log a warning. For
truncated queries, mark N+1 confidence as degraded (multiply by 0.5).

### 2.8 Concurrent DDL operations

The risk assessor gathers runtime context (active queries, lock state) at a
point in time. But DDL lock acquisition is not instantaneous. Between the
time the assessor runs and the time the advisory is delivered, the lock state
may have changed completely.

More critically: what happens when two DDL statements are detected in the
same analysis cycle? The risk assessment for DDL-A does not account for
DDL-B's lock, and vice versa.

**Amendment**: Document the inherent time-of-check-to-time-of-use (TOCTOU)
limitation: "Risk assessments reflect a point-in-time snapshot. Concurrent
DDL operations may interact in ways not captured by individual assessments."
For multiple DDL statements in the same cycle, assess them independently but
flag them as a group: "2 ACCESS EXCLUSIVE DDL operations detected in the
same window on related tables. Risk of cascading lock contention is elevated."

### 2.9 N+1 detector with PgBouncer (shared backend PIDs)

The spec does not mention connection poolers. With PgBouncer in transaction
mode, multiple application sessions share backend PIDs. This does not
directly affect `pg_stat_statements` (which aggregates by queryid, not PID),
but it affects the parent-child correlation.

When PgBouncer multiplexes, a "parent" query from App-A and a "child" query
from App-B may appear correlated in `pg_stat_statements` because they
touch related tables with matching call ratios -- but they are actually
independent workloads.

**Amendment**: Document the limitation: "Connection poolers (PgBouncer,
Odyssey) may cause false positive N+1 correlations when unrelated
applications share the same database. Consider the `min_confidence`
threshold and manual suppression for environments with heavy multiplexing."
If `pg_stat_statements` tracks `toplevel` (PG14+), filter to
`toplevel = true` to reduce noise.

### 2.10 Matviews that depend on other matviews

A matview `mv_summary` that queries `mv_detail` creates a dependency chain.
Refreshing `mv_summary` before `mv_detail` produces stale results.

> "Track base table modification rates and compare to matview refresh times"

The `BaseTables` field is "extracted from definition." If the definition
references another matview, is that matview treated as a base table? The
staleness calculation would be wrong because matview modification rates are
not tracked in `pg_stat_user_tables` the same way (matviews have no
`n_tup_ins`/`n_tup_upd`/`n_tup_del` counters from DML -- only from
REFRESH).

**Amendment**: Add matview dependency detection:
1. Parse `BaseTables` by joining `pg_depend` and `pg_rewrite` to find all
   relations referenced by the matview definition.
2. If any referenced relation is itself a matview, record the dependency.
3. When computing staleness for a dependent matview, propagate staleness:
   if `mv_detail` is stale, `mv_summary` is at least as stale.
4. When auto-refreshing, refresh dependencies first (topological sort).
5. Detect circular matview dependencies and flag as an anti-pattern.

---

## 3. Contradictions

### 3.1 Sequence monitor: Section 2.3.1 thresholds vs. Section 5.4 thresholds

Section 2.3.1 defines `lint_sequence_overflow`:
> "critical (>85%), warning (>60%)"

Section 5.4 defines the Sequence Overflow Monitor thresholds:
> ">50% info, >75% warning, >85% critical, >95% critical"

These are the same feature described in two places with DIFFERENT thresholds.
Is `lint_sequence_overflow` the lint rule and Section 5 the monitor? They
appear to be the same detection with different severity mappings. 60% warning
vs 75% warning is a significant difference.

**Amendment**: Unify. Section 5 (Sequence Overflow Monitor) is the canonical
definition. Remove `lint_sequence_overflow` from Section 2.3.1 -- it is
not a separate lint rule, it IS the sequence monitor. If keeping both (lint
rule for point-in-time findings, monitor for time-to-exhaustion projection),
define clearly: the lint rule fires on current percentage with thresholds
from Section 5.4, and the monitor adds time-to-exhaustion projection on top.
Use consistent thresholds.

### 3.2 Package location: spec says `internal/schema/lint` but Section 5 implies `internal/schema/lint` for sequences too

The Feature Summary table says Sequence Overflow Monitor's new package is
`internal/schema/lint`:

> | 5 | Sequence Overflow Monitor | P0 | 2 days | `internal/schema/lint` |

But Section 5.3 defines a `SequenceMonitor` type with a `TimeToExhaustion()`
method that tracks deltas across cycles. This is stateful monitoring, not
a stateless lint rule. Meanwhile, the existing codebase already has
`rules_sequence.go` in `internal/analyzer/` with `ruleSequenceExhaustion`.

This means sequence exhaustion would exist in THREE places:
1. `internal/analyzer/rules_sequence.go` (existing, fires per snapshot)
2. `internal/schema/lint/` (new lint rule per Section 2.3.1)
3. `internal/schema/lint/` (new monitor per Section 5)

**Amendment**: Consolidate. The existing `rules_sequence.go` in
`internal/analyzer/` handles per-cycle detection and should remain as-is.
Section 5's `SequenceMonitor` (time-to-exhaustion projection) belongs in
`internal/schema/lint/` as an EXTENSION of the lint rule, not a separate
monitor. Remove the duplication. The analyzer rule continues to fire
findings; the lint package adds the time-to-exhaustion projection and the
expanded threshold tiers (50/75/85/95%).

### 3.3 `ddl_set_not_null` safe alternative contradicts PG12+ behavior

> `ddl_set_not_null`: `ALTER COLUMN ... SET NOT NULL` on table > 10K rows
> Safe Alternative: "CHECK NOT VALID + VALIDATE + SET NOT NULL (PG12+)"

In PG12+, `SET NOT NULL` with an existing valid CHECK constraint
`CHECK (col IS NOT NULL)` is metadata-only -- it does NOT scan the table.
The spec labels the operation as requiring ACCESS EXCLUSIVE with a scan,
but the safe alternative (add CHECK NOT VALID + VALIDATE + SET NOT NULL)
results in SET NOT NULL being... metadata-only. The rule should fire for
the INITIAL `SET NOT NULL` without an existing CHECK, not label `SET NOT
NULL` itself as always dangerous.

**Amendment**: Reword the rule: "Flag `SET NOT NULL` when no valid
`CHECK (column IS NOT NULL)` constraint exists on the column. If the CHECK
exists and is validated, `SET NOT NULL` is metadata-only and safe." The
classifier must query `pg_constraint` for an existing CHECK before flagging.

### 3.4 `migration.mode: "advisory"` vs. `migration.mode: "active"` but Section 1.8 says active is "Future"

The config in Section 1.3 defines `mode: "advisory" # advisory | active`,
implying both modes are valid in v0.10. But Section 1.8 says:

> "This is a future capability gated on the trust ramp. v0.10 ships with
> advisory mode only."

**Amendment**: Remove `active` from the v0.10 config. Define
`mode: "advisory"` as the only valid value. If `active` is set, log a
warning and fall back to `advisory`. Add `active` mode in the version that
implements it.

---

## 4. Under-Specified Interfaces

### 4.1 REST API response shapes not defined

Section 7 lists endpoints but provides zero response body definitions:

> ```
> GET /api/schema/findings  # list findings, filterable
> GET /api/schema/health    # summary health score
> ```

What does the response look like? What are the filter parameters?
What pagination scheme (offset/limit? cursor?)? What is the "health score"
format?

**Amendment**: Define response schemas. Example:
```json
// GET /api/schema/findings?severity=critical&category=safety&table=orders&page=1&per_page=50
{
  "findings": [Finding],
  "total": 42,
  "page": 1,
  "per_page": 50
}

// GET /api/schema/health
{
  "score": 0.72,
  "critical": 3,
  "warning": 12,
  "info": 27,
  "last_scan": "2026-04-13T10:30:00Z",
  "top_issues": [Finding, Finding, Finding]
}
```

### 4.2 `POST /api/migration/assess` -- synchronous or async?

> Body: `{ "sql": "ALTER TABLE ..." }`
> Response: DDLRisk object

If the SQL references a large table, the risk assessment queries
`pg_stat_activity`, `pg_locks`, and replication status. This could take
seconds. Is this endpoint synchronous (blocks until assessment completes)
or async (returns a job ID)?

What about multi-statement SQL? Can a user submit
`ALTER TABLE a ...; ALTER TABLE b ...;`?

**Amendment**: Define as synchronous with a timeout. The assessment should
complete in < 5 seconds (it's querying catalog views, not executing DDL).
Add a `timeout_ms` parameter (default: 5000). For multi-statement input,
split on semicolons (respecting string literals) and assess each statement
independently, returning an array of `DDLRisk` objects. Define error
responses:
```json
// 400 Bad Request
{ "error": "unsupported_statement", "detail": "Cannot parse: ..." }
// 408 Timeout
{ "error": "assessment_timeout", "detail": "..." }
```

### 4.3 `POST /api/matviews/:name/refresh` -- what does `:name` look like?

Matviews are schema-qualified. Is the route parameter `public.my_matview`
or just `my_matview`? How are dots handled in URL paths?

**Amendment**: Use the format `/api/matviews/:schema/:name/refresh`. If
schema is omitted, default to `search_path` resolution. Alternatively,
accept schema as a query parameter:
`POST /api/matviews/:name/refresh?schema=analytics`.

### 4.4 N+1 detection `POST /api/n-plus-one/:id/suppress` -- what is `:id`?

The `NplusOneCandidate` struct uses `QueryID int64` (from
pg_stat_statements). But pg_stat_statements `queryid` changes across
stats resets and PG version upgrades. Suppressing by `queryid` would break
after a stats reset.

**Amendment**: Use a stable identifier for N+1 patterns. Recommended: hash
the normalized query text (which pg_stat_statements already normalizes).
Store suppressions in `sage.n_plus_one_suppressions` keyed by query text
hash, not by queryid. The suppression survives stats resets.

### 4.5 `lint_jsonb_in_joins` detection method is a hand-wave

> "Cross-reference JSONB columns with `pg_stat_statements` query patterns"

This is not a detection method, it's a wish. `pg_stat_statements` stores
normalized query text. Detecting that a JSONB column is used in a JOIN or
WHERE clause requires:
1. Parsing the normalized SQL to identify column references
2. Resolving those references to their actual column types via the catalog
3. Checking for GIN indexes on those columns

This is essentially building a query planner. The spec provides no detail.

**Amendment**: Either (a) downgrade this to a Tier 2 LLM rule: send the top
50 slowest queries to the LLM with schema context and ask "which of these
use JSONB columns in JOINs or WHEREs without GIN indexes?", or (b) simplify
the detection: find all JSONB columns, check if they have GIN indexes, and
flag columns WITHOUT GIN indexes on tables that appear in any slow query
(above `slow_query_threshold_ms`). This is a rough heuristic but
implementable without a SQL parser.

### 4.6 `ComputeImpact()` method signature takes `*pgxpool.Pool` but Finding is a data struct

> ```go
> func (f *Finding) ComputeImpact(pool *pgxpool.Pool) {
> ```

This puts database access on a data struct method, which is an
anti-pattern. The Finding type is also persisted to the database and
serialized as JSON. Having it hold a pool reference (or accept one in a
method) creates confusing ownership.

**Amendment**: Move impact computation to the Linter or a dedicated
`ImpactComputer` type:
```go
func (l *Linter) computeImpact(ctx context.Context, f *Finding) error
```
The Finding remains a pure data struct.

---

## 5. Dependency Risks

### 5.1 DDL detection depends on logwatch infrastructure from v0.9.1

The spec says DDL detection extends logwatch:

> "extending the v0.9.1 logwatch infrastructure"

The v0.9.1 logwatch code exists (`internal/logwatch/` with classifier,
parser, tailer, watcher). However, the v0.9.1 spec review
(`spec-review-edge-cases.md`) identified 12 P0 issues that "MUST be
resolved before implementation." It is unclear whether those P0 issues have
been resolved.

**Amendment**: Before starting v0.10 DDL detection, verify that all P0
items from the v0.9.1 spec review are resolved. Specifically: dedup key
definition (1.2), Drain() lifecycle (1.3), cold-start seek-to-end (2.5),
multi-line buffering (3.1), rate limiting (4.4), and fleet mode dedup (6.7).
If any P0 is unresolved, DDL log detection may build on a broken foundation.

### 5.2 `sage.schema_findings` table depends on schema migration infrastructure

The spec defines a new table with indexes and a UNIQUE constraint using
COALESCE expressions. This requires the existing schema bootstrap
(`internal/schema/bootstrap.go`) to be extended.

The existing `bootstrap.go` creates the `sage` schema and `sage.incidents`
table. Adding `sage.schema_findings` requires:
1. Extension of the migration system
2. Handling the case where the table already exists (idempotency)
3. Future schema evolution (adding columns to `schema_findings`)

**Amendment**: Add `sage.schema_findings` creation to the existing
`bootstrap.go` migration sequence, after `sage.incidents`. Use
`CREATE TABLE IF NOT EXISTS`. Add a version tracking mechanism for the
schema_findings table to support future column additions.

### 5.3 N+1 detection depends on collector already having QueryStats with `rows` field

The existing `QueryStats` struct has a `Rows int64` field. The N+1 detection
SQL (Section 3.2) uses `rows::float / calls` which is `total_rows / calls`.
But `pg_stat_statements.rows` is the total rows processed (returned + affected),
and its semantics differ between PG13 and PG14+.

In PG13, `rows` is the total rows returned/affected. In PG14+,
`pg_stat_statements` added `toplevel` filtering. The N+1 detector should
use `toplevel = true` queries only (PG14+), or the call counts will
include nested function calls.

**Amendment**: Add a PG version gate: when PG >= 14, add `AND toplevel` to
the N+1 candidate query. When PG < 14, document degraded accuracy.

### 5.4 Dashboard UI changes assume frontend framework capabilities

Section 8 describes "Summary bar", "Findings table", "Detail drawer",
"Suppress button" -- all interactive UI elements. The spec does not reference
the existing dashboard technology stack.

**Amendment**: Document the frontend technology used by the existing
dashboard (appears to be a compiled SPA based on
`api/dist/assets/index-*.js`). Specify whether the new pages are additions
to the existing SPA or new routes. If the dashboard uses a framework
(React, Vue, etc.), specify component structure.

### 5.5 LLM prompt templates depend on existing Tier 2 infrastructure

Sections 1.7, 3.4, and 4.5 define LLM prompt templates that "use the same
token budget and rate limiting as the existing Tier 2 RCA system." The
existing Tier 2 system (`internal/rca/tier2.go`) has specific prompt
construction, response parsing, and error handling patterns.

**Amendment**: Define how the new prompts integrate with the existing LLM
pipeline. Specifically:
1. Do they share the same rate limiter instance?
2. What happens when schema intelligence LLM calls contend with RCA Tier 2
   calls?
3. What is the priority order? (RCA Tier 2 should have priority over
   schema advisory LLM calls)
4. Define the JSON response schema for each new prompt template.

---

## 6. Missing Version-Specific Behavior

### 6.1 `REINDEX CONCURRENTLY` (PG12+)

> `ddl_reindex_not_concurrent`: "REINDEX without CONCURRENTLY (PG12+)"

The rule correctly notes PG12+. But the spec doesn't handle PG11 or below.
If pg_sage connects to PG11, the safe alternative ("use `REINDEX
CONCURRENTLY`") is invalid advice.

**Amendment**: Gate the safe alternative on PG version. For PG < 12,
the recommendation should be "Schedule REINDEX during maintenance window"
instead of "use CONCURRENTLY."

### 6.2 `ADD COLUMN DEFAULT` behavior changed in PG11

Before PG11, `ALTER TABLE ADD COLUMN ... DEFAULT <expr>` ALWAYS rewrites
the table. PG11+ only rewrites for volatile defaults.

The rule `ddl_add_column_volatile_default` implicitly assumes PG11+
behavior. On PG10 or PG11 (if pg_sage supports it), ANY default causes a
rewrite.

**Amendment**: Gate the rule on PG version:
- PG < 11: Flag ALL `ADD COLUMN ... DEFAULT` as requiring rewrite
- PG >= 11: Flag only volatile defaults (per 1.3 above)
If pg_sage's minimum supported PG version is 12, state this explicitly and
remove PG11 concerns.

### 6.3 `SET NOT NULL` metadata-only optimization (PG12+)

As noted in 3.3, PG12+ can skip the table scan for `SET NOT NULL` if a
valid CHECK constraint exists. PG11 always scans.

**Amendment**: Document minimum PG version for each safe alternative.
Add a `min_pg_version` field to each DDL classification rule. The advisor
output should not recommend a safe alternative that requires a PG version
newer than the target database.

### 6.4 `CREATE INDEX CONCURRENTLY ... INCLUDE` (PG11+)

Covering indexes (`INCLUDE` clause) were added in PG11. If the LLM
recommends a covering index as an alternative to a matview (Section 4.5),
the recommendation is invalid on PG10.

**Amendment**: Include PG version in all LLM prompt templates so the LLM
can tailor recommendations to the available feature set. The spec's DDL
safety prompt (Section 1.7) already includes `{{.Version}}` -- verify all
other prompts do too.

### 6.5 `pg_stat_statements.toplevel` column (PG14+)

The N+1 detection query uses `pg_stat_statements` but does not reference
the `toplevel` column added in PG14. On PG14+, without filtering
`toplevel = true`, the query counts include calls from within functions,
triggers, and procedures -- inflating child query call counts and producing
false N+1 positives.

**Amendment**: Add version-gated SQL:
```sql
-- PG14+: add AND toplevel = true
-- PG12-13: omit toplevel filter, accept degraded accuracy
```

### 6.6 `pg_sequences` view behavior varies

The spec's sequence detection query (Section 5.2) uses `pg_sequences` which
was introduced in PG10. However, `last_value` is NULL for sequences that
have never been used. The spec filters `WHERE last_value IS NOT NULL`, which
correctly handles this.

But: on PG10-15, `pg_sequences.last_value` requires ownership or
`pg_read_all_sequences` role (granted to `pg_monitor` in PG15+). On
managed services, the monitoring role may not have this privilege, and
`last_value` returns NULL even for used sequences.

**Amendment**: Document the permission requirement: "Sequence monitoring
requires the pg_sage database role to have `pg_read_all_sequences` privilege
(PG15+: included in `pg_monitor`) or ownership of the sequences. On
managed services, ensure the role has been granted `pg_monitor` or
`pg_read_all_sequences`." Add a fallback: if `last_value` is NULL for
all sequences, log a warning about insufficient privileges.

### 6.7 `log_destination = 'jsonlog'` (PG15+)

The DDL log detection extends logwatch which supports jsonlog and csvlog.
jsonlog was added in PG15. The spec does not specify minimum PG version for
DDL log detection.

**Amendment**: DDL log detection via jsonlog requires PG15+. Via csvlog,
PG12+ (csvlog has been available since PG8.0, but pg_sage's minimum is
likely PG12). Document this. If the database is PG12-14 and only csvlog
is available, DDL detection works but uses the csvlog parser.

---

## 7. Additional Implementation Gaps

### 7.1 No deduplication for schema lint findings across cycles

The `sage.schema_findings` table has a UNIQUE constraint and the spec says
existing findings have `last_seen` updated. But the Finding struct has no
stable identity for UPDATE matching beyond the UNIQUE key
`(rule_id, schema_name, table_name, COALESCE(column_name, ''), COALESCE(index_name, ''))`.

What happens when a finding's `description` or `suggestion` changes between
cycles (e.g., the table grew and the impact description changed)? Is the
existing row updated with the new description, or is it a new finding?

**Amendment**: The UNIQUE key defines identity. On upsert (INSERT ON
CONFLICT), update `last_seen`, `description`, `suggestion`, `suggested_sql`,
`table_size`, and `impact_score`. The `first_seen` is preserved. Use
`INSERT ... ON CONFLICT DO UPDATE`. Specify this explicitly.

### 7.2 No suppression persistence for N+1 patterns

The API defines `POST /api/n-plus-one/:id/suppress` but there is no
persistence table for N+1 suppressions or N+1 detections.

**Amendment**: Add:
```sql
CREATE TABLE IF NOT EXISTS sage.n_plus_one_detections (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    query_hash    BIGINT NOT NULL,
    query_text    TEXT NOT NULL,
    parent_hash   BIGINT,
    parent_text   TEXT,
    calls         BIGINT,
    mean_exec_ms  REAL,
    rows_per_call REAL,
    confidence    REAL,
    est_savings_ms REAL,
    first_seen    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen     TIMESTAMPTZ NOT NULL DEFAULT now(),
    suppressed    BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (query_hash)
);
```

### 7.3 Missing `lint_bloated_table` interaction with autovacuum tuning

The `lint_bloated_table` rule fires when dead tuple ratio exceeds 30%.
pg_sage already has an autovacuum tuner (`internal/tuner/`). A bloated table
finding should cross-reference with the tuner: if autovacuum is already
tuned aggressively for this table, the finding should note "autovacuum is
configured but not keeping up" vs. "autovacuum may need tuning."

**Amendment**: Add cross-reference: the lint rule queries
`pg_stat_user_tables.last_autovacuum` and the current autovacuum per-table
settings (`pg_class.reloptions`) to add context to the finding. If
autovacuum ran recently and bloat persists, the suggestion should be
"consider pg_repack or manual VACUUM FULL during maintenance" rather than
"tune autovacuum."

### 7.4 No rate limiting on schema lint incident generation

The linter runs every hour. Each run could produce 50+ findings. If all
critical findings generate incidents, the RCA incident table could be
flooded with 50 new incidents every hour (if nothing is fixed).

**Amendment**: Schema lint incidents should deduplicate against existing
open incidents. If an incident for `(source: "schema_lint", rule_id, table)`
already exists and is unresolved, do not create a new one -- update the
existing incident's `last_seen` equivalent. Alternatively, schema lint
findings produce incidents only on FIRST detection (new finding) and on
severity ESCALATION (warning -> critical).

---

## 8. Summary of Recommended Spec Amendments

| # | Section | Priority | Effort |
|---|---------|----------|--------|
| 1.1 | DDL detection prerequisites | P0 | 1 paragraph |
| 1.2 | Threshold naming clarity | P1 | Rename config keys |
| 1.3 | Volatile default definition | P0 | 1 paragraph |
| 1.4 | SQL parser strategy | P0 | 1 paragraph |
| 1.5 | Risk score floor | P0 | Formula fix |
| 1.6 | Finding type relationship | P0 | 1 paragraph |
| 1.7 | N+1 ratio tolerance | P1 | 1 paragraph |
| 1.8 | Matview refresh tracking | P0 | Table DDL + 1 paragraph |
| 2.1 | pg_stat_statements not enabled | P0 | 1 paragraph |
| 2.2 | Standby behavior | P0 | 1 section |
| 2.3 | Partitioned tables | P0 | 1 section |
| 2.4 | Temp table exclusion | P1 | 1 line |
| 2.5 | Non-public schema handling | P0 | 1 paragraph |
| 2.6 | Managed service compat | P0 | 1 section |
| 2.7 | Query text truncation | P1 | 1 paragraph |
| 2.8 | Concurrent DDL TOCTOU | P2 | 1 paragraph |
| 2.9 | PgBouncer N+1 false positives | P1 | 1 paragraph |
| 2.10 | Matview dependency chains | P1 | 1 paragraph |
| 3.1 | Sequence threshold contradiction | P0 | Unify thresholds |
| 3.2 | Sequence package location | P0 | Consolidate |
| 3.3 | SET NOT NULL safe alternative | P0 | Rewrite rule |
| 3.4 | Active mode future-gating | P1 | Remove from config |
| 4.1 | REST response schemas | P0 | 1 page |
| 4.2 | Assessment endpoint semantics | P1 | 1 paragraph |
| 4.3 | Matview route schema | P1 | Route definition |
| 4.4 | N+1 suppression ID stability | P1 | 1 paragraph |
| 4.5 | JSONB-in-joins detection method | P0 | Redefine approach |
| 4.6 | ComputeImpact method ownership | P1 | Signature change |
| 5.1 | Logwatch P0 dependencies | P0 | Verification |
| 5.2 | Schema bootstrap extension | P1 | 1 paragraph |
| 5.3 | PG14+ toplevel filtering | P1 | Version gate |
| 5.4 | Dashboard tech stack | P2 | 1 paragraph |
| 5.5 | LLM pipeline integration | P1 | 1 paragraph |
| 6.1 | REINDEX CONCURRENTLY version gate | P1 | 1 line |
| 6.2 | ADD COLUMN DEFAULT version gate | P1 | 1 paragraph |
| 6.3 | SET NOT NULL version gate | P1 | 1 paragraph |
| 6.4 | INCLUDE clause version gate | P2 | 1 line |
| 6.5 | toplevel column version gate | P1 | SQL variant |
| 6.6 | pg_sequences permissions | P1 | 1 paragraph |
| 6.7 | jsonlog minimum PG version | P2 | 1 line |
| 7.1 | Finding upsert semantics | P1 | 1 paragraph |
| 7.2 | N+1 persistence table | P0 | Table DDL |
| 7.3 | Bloat + autovacuum cross-ref | P2 | 1 paragraph |
| 7.4 | Lint incident dedup | P0 | 1 paragraph |

**P0 count**: 16 items -- these MUST be resolved before implementation.
**P1 count**: 19 items -- these should be resolved before implementation.
**P2 count**: 5 items -- can be addressed during implementation.
