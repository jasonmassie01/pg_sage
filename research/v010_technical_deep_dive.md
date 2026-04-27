# pg_sage v0.10 "Schema Intelligence" — Technical Deep Dive

> Research compiled for pg_sage autonomous DBA agent implementation.
> Covers PG12–PG18 where noted. All SQL tested against PostgreSQL conventions.

---

## Table of Contents

1. [DDL Locking Internals](#1-ddl-locking-internals)
2. [Schema Anti-Patterns](#2-schema-anti-patterns)
3. [N+1 Detection at Database Level](#3-n1-detection-at-database-level)
4. [Materialized View Intelligence](#4-materialized-view-intelligence)
5. [PostgreSQL-Specific Migration Patterns](#5-postgresql-specific-migration-patterns)
6. [Version-Specific DDL Improvements](#6-version-specific-ddl-improvements)

---

## 1. DDL Locking Internals

### 1.1 Lock Levels by ALTER TABLE Operation

PostgreSQL uses eight table-level lock modes. The critical ones for DDL are:

| Lock Mode | Conflicts With | Blocks SELECT? | Blocks INSERT/UPDATE/DELETE? |
|---|---|---|---|
| ACCESS SHARE | ACCESS EXCLUSIVE | No | No |
| ROW SHARE | EXCLUSIVE, ACCESS EXCLUSIVE | No | No |
| ROW EXCLUSIVE | SHARE, SHARE ROW EXCLUSIVE, EXCLUSIVE, ACCESS EXCLUSIVE | No | No |
| SHARE UPDATE EXCLUSIVE | SHARE UPDATE EXCLUSIVE, SHARE, SHARE ROW EXCLUSIVE, EXCLUSIVE, ACCESS EXCLUSIVE | No | No |
| SHARE | ROW EXCLUSIVE, SHARE UPDATE EXCLUSIVE, SHARE ROW EXCLUSIVE, EXCLUSIVE, ACCESS EXCLUSIVE | No | **Yes** |
| SHARE ROW EXCLUSIVE | ROW EXCLUSIVE, SHARE UPDATE EXCLUSIVE, SHARE, SHARE ROW EXCLUSIVE, EXCLUSIVE, ACCESS EXCLUSIVE | No | **Yes** |
| EXCLUSIVE | All except ACCESS SHARE | No | **Yes** |
| ACCESS EXCLUSIVE | **All modes** | **Yes** | **Yes** |

### 1.2 ALTER TABLE Subcommands — Complete Lock Level Reference

**ACCESS EXCLUSIVE (blocks everything including SELECT):**

| Operation | Requires Rewrite? | Notes |
|---|---|---|
| `ADD COLUMN` (volatile DEFAULT) | **Yes** | e.g., `DEFAULT clock_timestamp()` |
| `ADD COLUMN` (non-volatile DEFAULT) | **No** | PG11+: stored in `pg_attribute.attmissingval` |
| `ADD COLUMN` (no DEFAULT, nullable) | **No** | Instant metadata-only change |
| `DROP COLUMN` | No | Marks column as dropped; space reclaimed on rewrite |
| `ALTER COLUMN SET DATA TYPE` | **Usually yes** | Unless binary coercible (see Section 5.6) |
| `ALTER COLUMN SET/DROP DEFAULT` | No | Metadata-only change |
| `ALTER COLUMN SET/DROP NOT NULL` | No (scan only) | Scans table to verify, no rewrite |
| `ALTER COLUMN ADD IDENTITY` | **Yes** | Adds identity sequence |
| `ALTER COLUMN DROP IDENTITY` | No | |
| `ALTER COLUMN SET EXPRESSION` | **Yes** | Stored generated columns |
| `ALTER COLUMN DROP EXPRESSION` | No | |
| `ALTER COLUMN SET STORAGE` | No | |
| `ALTER COLUMN SET COMPRESSION` | No | |
| `ADD CONSTRAINT` (CHECK) | No (scan only) | Validates existing rows |
| `DROP CONSTRAINT` | No | |
| `DISABLE/ENABLE TRIGGER` | No | |
| `DISABLE/ENABLE RULE` | No | |
| `DISABLE/ENABLE ROW LEVEL SECURITY` | No | |
| `SET ACCESS METHOD` | **Yes** | PG15+ |
| `SET TABLESPACE` | **Yes** | Physical file move |
| `SET LOGGED/UNLOGGED` | **Yes** | |
| `INHERIT/NO INHERIT` | No | |
| `RENAME` | No | |

**SHARE ROW EXCLUSIVE (blocks writes to both tables, allows reads):**

| Operation | Notes |
|---|---|
| `ADD FOREIGN KEY` | Locks both source and referenced tables |
| `DISABLE/ENABLE REPLICA TRIGGER` | |

**SHARE UPDATE EXCLUSIVE (allows reads and writes, blocks DDL and VACUUM):**

| Operation | Notes |
|---|---|
| `SET STATISTICS` | Per-column stats target |
| `SET (storage_parameter)` | fillfactor, autovacuum params, etc. |
| `CLUSTER ON / SET WITHOUT CLUSTER` | |
| `VALIDATE CONSTRAINT` | Scans table but allows concurrent writes |
| `ATTACH PARTITION` | Scans to verify constraint; ACCESS EXCLUSIVE on partition only |

**Key insight for pg_sage**: When multiple subcommands are given in a single ALTER TABLE, the **strictest** lock required by any subcommand is acquired. pg_sage should recommend splitting operations to minimize lock scope.

### 1.3 Lock Queue Behavior — The Cascading Block Problem

This is the most dangerous aspect of PostgreSQL DDL and the core reason pg_sage must be lock-aware.

**How the lock queue works:**

```
Time 0: Transaction A starts: SELECT ... FROM orders (holds ACCESS SHARE, runs for 30s)
Time 1: Migration runs: ALTER TABLE orders ADD COLUMN ... (needs ACCESS EXCLUSIVE)
         → Queued behind A, waiting for ACCESS SHARE to release
Time 2: App sends: SELECT * FROM orders WHERE id = 5 (needs ACCESS SHARE)
         → Queued behind the ALTER TABLE, NOT behind A
Time 3: More SELECTs arrive → all queued behind ALTER TABLE
         → Table is effectively OFFLINE for all operations
```

**The lock queue is FIFO with conflict checking.** When transaction C arrives, it checks for conflicts with all transactions ahead of it in the queue, including the waiting ALTER TABLE. Since ACCESS SHARE conflicts with ACCESS EXCLUSIVE, C waits. This means a DDL statement that hasn't even started yet blocks all subsequent operations.

**Mitigation strategy — lock_timeout with retry:**

```sql
-- Pattern 1: Simple lock_timeout
SET lock_timeout TO '2s';
ALTER TABLE items ADD COLUMN last_update timestamptz;
-- If lock can't be acquired in 2s, statement fails, queue clears

-- Pattern 2: Transaction-level with explicit lock
BEGIN;
  SET LOCAL lock_timeout = '50ms';
  LOCK TABLE ONLY orders IN ACCESS EXCLUSIVE MODE;
  -- If acquired, proceed with DDL
  SET LOCAL statement_timeout = 0;  -- Allow DDL to run without time limit
  ALTER TABLE orders ADD COLUMN ...;
COMMIT;

-- Pattern 3: PL/pgSQL retry with exponential backoff and jitter
DO $do$
DECLARE
  lock_timeout CONSTANT text := '50ms';
  max_attempts CONSTANT int := 30;
  ddl_completed boolean := false;
  cap_ms bigint := 60000;
  base_ms bigint := 10;
  delay_ms bigint;
BEGIN
  PERFORM set_config('lock_timeout', lock_timeout, false);
  FOR i IN 1..max_attempts LOOP
    BEGIN
      ALTER TABLE orders ADD COLUMN new_col int4;
      ddl_completed := true;
      EXIT;
    EXCEPTION WHEN lock_not_available THEN
      RAISE WARNING 'attempt %/% failed, retrying', i, max_attempts;
      delay_ms := round(random() * least(cap_ms, base_ms * 2 ^ i));
      PERFORM pg_sleep(delay_ms::numeric / 1000);
    END;
  END LOOP;
  IF NOT ddl_completed THEN
    RAISE EXCEPTION 'failed to acquire lock after % attempts', max_attempts;
  END IF;
END $do$;
```

**pg_sage implementation notes:**
- Before recommending any DDL, check `pg_stat_activity` for long-running transactions
- If any transaction on the target table has been running > 1 minute, warn or defer
- Always recommend `SET lock_timeout` before DDL
- Monitor `pg_locks` for queued lock requests during DDL execution

### 1.4 Safe Online DDL Alternatives

| Dangerous Operation | Safe Alternative |
|---|---|
| `CREATE INDEX` | `CREATE INDEX CONCURRENTLY` |
| `ADD COLUMN ... DEFAULT val` (PG<11) | Add column, then backfill in batches |
| `ADD FOREIGN KEY` | `ADD ... NOT VALID` + `VALIDATE CONSTRAINT` |
| `ADD CHECK CONSTRAINT` | `ADD ... NOT VALID` + `VALIDATE CONSTRAINT` |
| `ADD NOT NULL` | Add CHECK constraint NOT VALID, validate, then SET NOT NULL (PG12+) |
| `ADD PRIMARY KEY` | `CREATE UNIQUE INDEX CONCURRENTLY` + `ADD CONSTRAINT ... USING INDEX` |
| `ALTER COLUMN TYPE` (rewrite) | Add new column, trigger-based dual-write, backfill, swap |
| `VACUUM FULL` | `pg_repack` or regular VACUUM |
| `REINDEX` | `REINDEX CONCURRENTLY` (PG12+) |
| `REFRESH MATERIALIZED VIEW` | `REFRESH MATERIALIZED VIEW CONCURRENTLY` (requires unique index) |

### 1.5 Foreign Key Creation Locking

Adding a foreign key acquires **SHARE ROW EXCLUSIVE** on **both** the source table and the referenced table. This blocks writes to both tables while the constraint is validated.

**Safe two-step pattern:**

```sql
-- Step 1: Add constraint without validation (instant, still blocks briefly)
ALTER TABLE orders
  ADD CONSTRAINT fk_orders_customer
  FOREIGN KEY (customer_id) REFERENCES customers(id)
  NOT VALID;
-- Takes SHARE ROW EXCLUSIVE briefly but skips full table scan

-- Step 2: Validate in separate transaction (SHARE UPDATE EXCLUSIVE only)
ALTER TABLE orders VALIDATE CONSTRAINT fk_orders_customer;
-- Scans table but allows concurrent writes
```

**Critical detail:** The NOT VALID constraint still enforces referential integrity for **new** rows (INSERT/UPDATE). It only skips validation of **existing** data. The subsequent VALIDATE CONSTRAINT checks existing data without blocking writes.

### 1.6 Enum Type Modifications

`ALTER TYPE ... ADD VALUE` is **safe for online use**:

- Acquires a brief lock on `pg_enum` catalog, NOT on data tables
- Does not require table rewrites
- New value is committed immediately

**Caveats:**
- Cannot use the new value in the **same transaction** that adds it (commit ADD VALUE first)
- Adding values with BEFORE/AFTER positioning may cause slower comparisons
- **Cannot remove** enum values directly (must create new type and swap)
- Cannot be used inside a transaction block (auto-commits)

```sql
-- Safe: adding a value
ALTER TYPE order_status ADD VALUE 'cancelled';
-- Commits immediately, usable in next transaction

-- Unsafe: removing a value (workaround)
-- 1. Create new enum without the value
CREATE TYPE order_status_new AS ENUM ('pending', 'shipped', 'delivered');
-- 2. Alter columns (requires ACCESS EXCLUSIVE + table rewrite)
ALTER TABLE orders ALTER COLUMN status TYPE order_status_new
  USING status::text::order_status_new;
-- 3. Drop old type
DROP TYPE order_status;
ALTER TYPE order_status_new RENAME TO order_status;
```

### 1.7 Partition Management DDL

**ATTACH PARTITION:**
- Acquires **SHARE UPDATE EXCLUSIVE** on the parent (allows reads and writes)
- Acquires **ACCESS EXCLUSIVE** on the partition being attached
- Scans the partition to validate the partition constraint
- Optimization: if the partition already has a matching CHECK constraint, the scan is skipped

```sql
-- Fast attach (pre-create CHECK constraint to skip validation scan)
ALTER TABLE measurements_y2024m01
  ADD CONSTRAINT partition_check
  CHECK (logdate >= '2024-01-01' AND logdate < '2024-02-01');

ALTER TABLE measurements
  ATTACH PARTITION measurements_y2024m01
  FOR VALUES FROM ('2024-01-01') TO ('2024-02-01');
-- Skips scan because CHECK already guarantees the constraint
```

**DETACH PARTITION (PG14+):**
- Without CONCURRENTLY: **ACCESS EXCLUSIVE** on parent (blocks everything)
- With CONCURRENTLY: **SHARE UPDATE EXCLUSIVE** on parent

```sql
-- PG14+: Non-blocking detach
ALTER TABLE measurements
  DETACH PARTITION measurements_y2023m01 CONCURRENTLY;
-- Cannot be run inside a transaction block
-- Cannot be used if parent has a default partition
```

**DETACH PARTITION CONCURRENTLY internals:**
1. First transaction: takes SHARE UPDATE EXCLUSIVE on parent and partition, marks partition as "being detached", commits
2. Waits for all transactions using the partitioned table to complete
3. Second transaction: takes SHARE UPDATE EXCLUSIVE on parent + ACCESS EXCLUSIVE on partition, completes detach

---

## 2. Schema Anti-Patterns

### 2.1 Missing Indexes on Foreign Key Columns

**The problem:** PostgreSQL automatically creates indexes on PRIMARY KEY and UNIQUE constraints but **not** on the referencing side of foreign keys. This causes:
- Full table scans on DELETE/UPDATE of parent rows (FK validation)
- Slow JOINs on the FK column
- Lock contention during bulk parent operations

**Detection query for pg_sage:**

```sql
-- Find foreign keys without indexes on the referencing columns
SELECT
  c.conrelid::regclass AS table_name,
  c.conname AS constraint_name,
  array_agg(a.attname ORDER BY x.n) AS fk_columns,
  pg_size_pretty(pg_relation_size(c.conrelid)) AS table_size
FROM pg_constraint c
CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS x(attnum, n)
JOIN pg_attribute a
  ON a.attrelid = c.conrelid AND a.attnum = x.attnum
WHERE c.contype = 'f'
  AND NOT EXISTS (
    SELECT 1 FROM pg_index i
    WHERE i.indrelid = c.conrelid
      AND (i.indkey::int2[])[0:array_length(c.conkey,1)-1]
          = c.conkey::int2[]
  )
GROUP BY c.conrelid, c.conname, c.conkey
ORDER BY pg_relation_size(c.conrelid) DESC;
```

### 2.2 Wrong Data Types

**timestamp vs timestamptz:**
- `timestamp` (without time zone) is a "picture of a clock" — no timezone awareness
- `timestamptz` stores internally as UTC, converts on display based on session timezone
- Both use 8 bytes — **no storage difference**
- Arithmetic across timezones or daylight saving boundaries gives wrong results with `timestamp`
- **Rule: Always use `timestamptz` unless storing wall-clock time (e.g., "the meeting is at 3pm regardless of timezone")**

```sql
-- Detection: find timestamp columns that should be timestamptz
SELECT
  c.relname AS table_name,
  a.attname AS column_name,
  format_type(a.atttypid, a.atttypmod) AS data_type
FROM pg_attribute a
JOIN pg_class c ON a.attrelid = c.oid
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE format_type(a.atttypid, a.atttypmod) = 'timestamp without time zone'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND a.attnum > 0
  AND NOT a.attisdropped;
```

**text vs varchar vs char:**
- `text`, `varchar`, and `varchar(n)` have **identical performance** in PostgreSQL
- `char(n)` pads with spaces, wastes storage, creates comparison bugs — **never use**
- `varchar(n)` adds a length check constraint — use only when the limit is a real business rule
- **Rule: Use `text` as default. Use `varchar(n)` only for real constraints (e.g., ISO country codes). Never use `char(n)`.**

**integer vs bigint:**
- `integer` (4 bytes): max 2,147,483,647
- `bigint` (8 bytes): max 9,223,372,036,854,775,807
- Sequence exhaustion is a **production incident** — inserts fail with "reached maximum value of sequence"
- Frequent INSERT/DELETE can exhaust sequences even on small tables (gaps from rolled-back transactions)
- Migrating integer to bigint requires a **table rewrite** (different binary representation)

```sql
-- Detection: sequences approaching exhaustion
SELECT
  seqrelid::regclass AS sequence_name,
  seqtypid::regtype AS data_type,
  CASE seqtypid::regtype::text
    WHEN 'smallint' THEN 32767
    WHEN 'integer'  THEN 2147483647
    WHEN 'bigint'   THEN 9223372036854775807
  END AS max_value,
  last_value,
  ROUND(
    100.0 * last_value /
    CASE seqtypid::regtype::text
      WHEN 'smallint' THEN 32767
      WHEN 'integer'  THEN 2147483647
      WHEN 'bigint'   THEN 9223372036854775807
    END, 2
  ) AS pct_used
FROM pg_sequences
WHERE last_value IS NOT NULL
ORDER BY pct_used DESC;
```

**Rule: Use `BIGINT GENERATED ALWAYS AS IDENTITY` for all new auto-incrementing columns. Monitor sequences at >50% usage.**

### 2.3 serial vs identity Columns

- `serial` is PostgreSQL-specific, creates a separate sequence with cumbersome ownership
- `identity` (PG10+) is SQL-standard, cleaner permission handling, easier management
- `GENERATED ALWAYS` prevents accidental manual value insertion
- `GENERATED BY DEFAULT` allows manual override

```sql
-- Detection: find serial columns that should be identity
SELECT
  c.relname AS table_name,
  a.attname AS column_name,
  pg_get_serial_sequence(c.relname, a.attname) AS sequence_name
FROM pg_attribute a
JOIN pg_class c ON a.attrelid = c.oid
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE pg_get_serial_sequence(c.relname, a.attname) IS NOT NULL
  AND n.nspname = 'public'
  AND NOT a.attidentity = 'a'
  AND NOT a.attidentity = 'd';
```

### 2.4 Unused Indexes

Every index has a **write amplification cost**: one logical INSERT triggers N+1 physical writes (1 heap + N indexes). Benchmarks show moving from 7 to 39 indexes causes ~58% throughput drop. A single unused index can reduce ingestion rate 10-15%.

```sql
-- Detection: unused indexes (exclude unique/PK-backing indexes)
SELECT
  schemaname || '.' || relname AS table,
  indexrelname AS index_name,
  pg_size_pretty(pg_relation_size(i.indexrelid)) AS index_size,
  idx_scan AS scans,
  idx_tup_read AS tuples_read,
  idx_tup_fetch AS tuples_fetched
FROM pg_stat_user_indexes ui
JOIN pg_index i ON ui.indexrelid = i.indexrelid
WHERE idx_scan = 0                     -- never scanned
  AND NOT i.indisunique                -- not backing a unique constraint
  AND NOT i.indisprimary               -- not a primary key
  AND pg_relation_size(i.indexrelid) > 8192  -- ignore tiny indexes
ORDER BY pg_relation_size(i.indexrelid) DESC;
```

**Warning:** Reset `pg_stat_user_indexes` counters after major PG upgrades (`pg_stat_reset()`), as counters don't survive `pg_upgrade`. Wait at least one full business cycle (1-4 weeks) before dropping indexes flagged as unused.

### 2.5 Duplicate and Overlapping Indexes

```sql
-- Detection: exact duplicate indexes
SELECT
  pg_size_pretty(sum(pg_relation_size(idx))::bigint) AS total_wasted,
  (array_agg(idx))[1] AS index_1,
  (array_agg(idx))[2] AS index_2,
  (array_agg(idx))[3] AS index_3
FROM (
  SELECT
    indexrelid::regclass AS idx,
    (indrelid::text || E'\n' || indclass::text || E'\n' ||
     indkey::text || E'\n' || coalesce(indexprs::text,'') ||
     E'\n' || coalesce(indpred::text,'')) AS key
  FROM pg_index
) sub
GROUP BY key
HAVING count(*) > 1
ORDER BY sum(pg_relation_size(idx)) DESC;

-- Detection: overlapping/redundant multi-column indexes
-- Index (a,b) makes index (a) redundant for most queries
SELECT
  a.indrelid::regclass AS table_name,
  a.indexrelid::regclass AS shorter_index,
  b.indexrelid::regclass AS longer_index,
  pg_size_pretty(pg_relation_size(a.indexrelid)) AS shorter_size
FROM pg_index a
JOIN pg_index b ON a.indrelid = b.indrelid
  AND a.indexrelid <> b.indexrelid
  AND (a.indkey::int2[])[0:array_length(a.indkey,1)-1]
      = (b.indkey::int2[])[0:array_length(a.indkey,1)-1]
  AND array_length(a.indkey,1) < array_length(b.indkey,1)
WHERE NOT a.indisunique  -- don't suggest dropping unique indexes
ORDER BY pg_relation_size(a.indexrelid) DESC;
```

### 2.6 Tables Without Primary Keys

**Problems caused:**
- Logical replication fails for UPDATE/DELETE — requires REPLICA IDENTITY
- Without a PK, PostgreSQL defaults to `REPLICA IDENTITY DEFAULT` which is "nothing" for tables without a PK
- `REPLICA IDENTITY FULL` (the fallback) uses entire row as key — extremely slow
- HOT (Heap Only Tuple) updates less effective
- No natural deduplication mechanism
- Many tools/frameworks assume PK existence

```sql
-- Detection: tables without primary keys
SELECT
  n.nspname AS schema_name,
  c.relname AS table_name,
  pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size,
  c.reltuples::bigint AS est_rows,
  CASE WHEN c.relreplident = 'd' THEN 'default (none)'
       WHEN c.relreplident = 'n' THEN 'nothing'
       WHEN c.relreplident = 'f' THEN 'full'
       WHEN c.relreplident = 'i' THEN 'index'
  END AS replica_identity
FROM pg_class c
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE c.relkind = 'r'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND NOT EXISTS (
    SELECT 1 FROM pg_index i
    WHERE i.indrelid = c.oid AND i.indisprimary
  )
ORDER BY c.reltuples DESC;
```

### 2.7 Wide Rows and TOAST Performance

PostgreSQL's TOAST mechanism kicks in when a row exceeds ~2KB. TOAST compresses or moves large values out-of-line to a separate TOAST table. The problem: `SELECT *` on a table with TOAST'd columns triggers additional I/O per row — the difference between 5ms and 850ms response times.

**Anti-pattern indicators:**
- Tables with many `text`, `jsonb`, `bytea` columns
- Average row size > 2KB
- `SELECT *` usage with unused large columns
- TOAST table significantly larger than main table

```sql
-- Detection: tables with heavy TOAST usage
SELECT
  c.relname AS table_name,
  pg_size_pretty(pg_relation_size(c.oid)) AS main_size,
  pg_size_pretty(pg_relation_size(t.oid)) AS toast_size,
  pg_size_pretty(pg_total_relation_size(c.oid)) AS total_size,
  ROUND(100.0 * pg_relation_size(t.oid) /
        NULLIF(pg_total_relation_size(c.oid), 0), 1) AS toast_pct
FROM pg_class c
JOIN pg_class t ON c.reltoastrelid = t.oid
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND pg_relation_size(t.oid) > 0
ORDER BY pg_relation_size(t.oid) DESC;
```

**Recommendation:** When TOAST table is >50% of total size, consider splitting large columns into a separate table joined on PK. This allows the main table to be scanned efficiently for most queries.

### 2.8 JSONB vs Normalized Schema

**When JSONB is appropriate:**
- Sparse/optional keys that vary across rows
- Schema-on-read patterns (event metadata, user preferences)
- Rapidly evolving schemas during early development
- Containment/existence queries (`@>`, `?`)

**When JSONB is an anti-pattern:**
- Core entity fields that are queried/filtered/joined on → normalize
- Large arrays or deeply nested structures → bloat, contention
- Fields central to hot query paths → use typed columns
- Storing uniform structured data to "avoid schema design"

**Performance facts:**
- PostgreSQL has **no statistics** for values inside JSONB — uses hardcoded estimate of 0.1% selectivity
- This can make queries ~2000x slower than equivalent normalized queries
- JSONB tables typically use ~2x the disk space vs normalized equivalent
- JSONB updates rewrite the **entire** JSONB value, not just the changed field

**Recommended hybrid pattern:**
```sql
-- Core fields as typed columns, flexible remainder as JSONB
CREATE TABLE products (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name text NOT NULL,
  category_id bigint REFERENCES categories(id),
  price numeric(10,2) NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  metadata jsonb DEFAULT '{}'::jsonb  -- flexible attributes
);

-- GIN index for JSONB containment queries
CREATE INDEX idx_products_metadata ON products USING gin (metadata);
```

```sql
-- Detection: tables heavily relying on JSONB
SELECT
  c.relname AS table_name,
  count(*) FILTER (WHERE format_type(a.atttypid, a.atttypmod) = 'jsonb') AS jsonb_cols,
  count(*) AS total_cols,
  ROUND(100.0 * count(*) FILTER (WHERE format_type(a.atttypid, a.atttypmod) = 'jsonb')
        / count(*), 1) AS jsonb_pct
FROM pg_attribute a
JOIN pg_class c ON a.attrelid = c.oid
JOIN pg_namespace n ON c.relnamespace = n.oid
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND a.attnum > 0 AND NOT a.attisdropped
  AND c.relkind = 'r'
GROUP BY c.relname
HAVING count(*) FILTER (WHERE format_type(a.atttypid, a.atttypmod) = 'jsonb') > 0
ORDER BY jsonb_pct DESC;
```

### 2.9 Enum Types vs Lookup Tables

| Aspect | Enum | Lookup Table |
|---|---|---|
| Storage | 4 bytes (internal integer) | FK integer (4-8 bytes) |
| Adding values | Easy (`ALTER TYPE ADD VALUE`) | Easy (`INSERT`) |
| Removing values | **Impossible** without type swap | Easy (`DELETE`) |
| Renaming values | `ALTER TYPE RENAME VALUE` (PG10+) | `UPDATE` |
| Query readability | Values inline in results | Requires JOIN |
| Ordering | Built-in enum ordering | Requires explicit sort column |
| Referential integrity | Enforced by type system | Enforced by FK |
| Statistics | Known, small cardinality | Standard table stats |

**Rule:** Use enums for truly stable, small value sets (< 20 values, rarely change). Use lookup tables for anything that changes, needs metadata (description, display order), or grows over time.

### 2.10 Missing NOT NULL Constraints

**Detection:**

```sql
-- Columns that are nullable but have zero NULLs (candidates for NOT NULL)
SELECT
  schemaname || '.' || tablename AS table_name,
  attname AS column_name,
  null_frac,
  n_distinct,
  most_common_vals
FROM pg_stats
WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
  AND null_frac = 0
  AND NOT attname = ANY(
    -- Exclude columns that are already NOT NULL
    SELECT a.attname
    FROM pg_attribute a
    JOIN pg_class c ON a.attrelid = c.oid
    JOIN pg_namespace n ON c.relnamespace = n.oid
    WHERE n.nspname || '.' || c.relname = schemaname || '.' || tablename
      AND a.attname = pg_stats.attname
      AND a.attnotnull
  )
ORDER BY schemaname, tablename, attname;
```

### 2.11 Missing CHECK Constraints

Application-level validation alone is insufficient. CHECK constraints:
- Enforce data integrity regardless of which client writes data
- Prevent bugs from `update_attribute`-style methods that skip validation
- Are evaluated only during INSERT/UPDATE (minimal overhead)
- Survive application code changes

**Common missing constraints pg_sage should recommend:**
- Positive values: `CHECK (price > 0)`
- Non-empty strings: `CHECK (length(trim(name)) > 0)`
- Valid email patterns: `CHECK (email ~* '^[^@]+@[^@]+\.[^@]+$')`
- Date ranges: `CHECK (end_date > start_date)`
- Status values: `CHECK (status IN ('active', 'inactive', 'pending'))`

---

## 3. N+1 Detection at Database Level

### 3.1 Can We Detect N+1 from pg_stat_statements?

**Yes, with caveats.** pg_stat_statements normalizes queries by replacing literal values with parameters (`$1`, `$2`), which is exactly what makes N+1 detection possible — all N iterations of the "child" query map to the same `queryid`.

**The N+1 signature in pg_stat_statements:**

| Metric | Parent Query | N+1 Child Query |
|---|---|---|
| `calls` | Low (1 per request) | Very high (N per request) |
| `mean_exec_time` | Higher (returns list) | Very low (returns 1 row) |
| `total_exec_time` | Moderate | Very high (dominates) |
| `rows` | High (N rows) | Low (1 row per call) |
| Ratio: calls/rows | ~1/N | ~1/1 |

**Detection query:**

```sql
-- Candidate N+1 queries: high calls, low mean_exec_time, low rows/call
SELECT
  queryid,
  substr(query, 1, 100) AS query_preview,
  calls,
  ROUND(mean_exec_time::numeric, 3) AS mean_ms,
  ROUND(total_exec_time::numeric, 1) AS total_ms,
  rows,
  ROUND(rows::numeric / NULLIF(calls, 0), 1) AS rows_per_call,
  ROUND(total_exec_time / NULLIF(
    (SELECT sum(total_exec_time) FROM pg_stat_statements), 0
  ) * 100, 2) AS pct_total_time
FROM pg_stat_statements
WHERE calls > 1000              -- executed many times
  AND mean_exec_time < 5        -- each call is fast (< 5ms)
  AND rows / NULLIF(calls, 0) <= 1  -- returns 0-1 rows per call
ORDER BY total_exec_time DESC
LIMIT 20;
```

### 3.2 Temporal Correlation with pg_stat_activity

For real-time detection, pg_sage can snapshot `pg_stat_activity` to detect N+1 patterns as they happen:

```sql
-- Snapshot active queries to detect repetitive patterns
-- Run this every 100ms for 2 seconds to build a sample
SELECT
  pid,
  query,
  query_start,
  state,
  wait_event_type,
  wait_event,
  backend_xid,
  backend_xmin
FROM pg_stat_activity
WHERE state = 'active'
  AND query NOT LIKE '%pg_stat_activity%'
  AND datname = current_database();
```

**Detection heuristic for pg_sage:**
1. Sample `pg_stat_activity` at 100ms intervals over a 2-second window
2. Group by normalized query text (strip literals)
3. If the same normalized query appears > 10 times in the window from the same `application_name` or `client_addr`, flag as potential N+1
4. Cross-reference with pg_stat_statements `calls` delta over the same period

### 3.3 auto_explain for N+1 Confirmation

```sql
-- Enable auto_explain to log plans for fast, frequent queries
ALTER SYSTEM SET auto_explain.log_min_duration = '1ms';
ALTER SYSTEM SET auto_explain.log_analyze = true;
ALTER SYSTEM SET auto_explain.log_nested_statements = true;
ALTER SYSTEM SET auto_explain.log_timing = true;
SELECT pg_reload_conf();
```

**Linking auto_explain to pg_stat_statements:** The `pg_logqueryid` extension adds `queryid` to auto_explain output, allowing correlation between execution plans and aggregated statistics.

### 3.4 Query Fingerprinting for N+1

pg_stat_statements normalizes queries into fingerprints:

```
-- These N+1 queries:
SELECT name FROM users WHERE id = 42;
SELECT name FROM users WHERE id = 87;
SELECT name FROM users WHERE id = 156;

-- All normalize to this single fingerprint:
SELECT name FROM users WHERE id = $1;
-- queryid: 0x1234567890ABCDEF (consistent hash)
```

The `queryid` is computed from the normalized query parse tree. All parameterized variants share the same `queryid`, making aggregation automatic.

### 3.5 pg_stat_monitor Advantages for N+1 Detection

Percona's `pg_stat_monitor` improves on `pg_stat_statements` for N+1 detection:
- **Time buckets**: Stats per configurable time interval (not just cumulative)
- **Actual parameters**: Can show real parameter values, not just `$1`
- **Client info**: Groups by `client_addr`, `application_name`
- **Histograms**: Execution time distribution per query

```sql
-- pg_stat_monitor: detect N+1 with time-bucketed data
SELECT
  bucket_start_time,
  queryid,
  substr(query, 1, 80) AS query,
  calls,
  mean_exec_time,
  client_ip
FROM pg_stat_monitor
WHERE calls > 100
  AND mean_exec_time < 2
  AND rows / NULLIF(calls, 0) <= 1
ORDER BY bucket_start_time DESC, calls DESC;
```

### 3.6 False Positive Scenarios

**Legitimate high-call/low-latency patterns that are NOT N+1:**
- Connection pooler health checks (`SELECT 1`)
- Cache-miss single-row lookups by PK (correctly batched at app level)
- Cursor-based pagination (fetching one page at a time is intentional)
- Event-driven architectures (each event legitimately triggers one query)
- Scheduled jobs running the same query per record (intentional batch processing)

**pg_sage should filter out:**
- Queries against `pg_catalog` or `information_schema`
- Queries matching known health check patterns
- Queries from monitoring/observability tools (identified by `application_name`)
- Queries with high `shared_blks_hit` ratio (fully cached — may be acceptable)

### 3.7 Recommended Fix Patterns for N+1

When pg_sage detects N+1, it should recommend:

```sql
-- Instead of N separate queries:
-- SELECT * FROM orders WHERE customer_id = $1;  (called N times)

-- Recommend batch query:
SELECT * FROM orders WHERE customer_id = ANY($1::bigint[]);

-- Or JOIN-based approach:
SELECT o.*
FROM customers c
JOIN orders o ON o.customer_id = c.id
WHERE c.region = 'west';
```

---

## 4. Materialized View Intelligence

### 4.1 When to Recommend: Matview vs Covering Index vs Query Rewrite

**Decision matrix for pg_sage:**

| Signal | Recommendation |
|---|---|
| Expensive aggregation/GROUP BY, tolerates staleness | Materialized view |
| Multi-table JOIN result used read-heavy | Materialized view |
| Single-table query, needs all columns returned | Covering index (INCLUDE) |
| Single-table, filter + few columns | Partial index or expression index |
| Query can be rewritten with CTEs or window functions | Query rewrite |
| Real-time requirement, no staleness tolerance | Covering index or query optimization |
| Write-heavy table, aggregation needed | TimescaleDB continuous aggregate |

**Matview is NOT appropriate when:**
- Source data changes frequently AND staleness is unacceptable
- The query is fast enough with proper indexing
- Storage is constrained (matview duplicates data)
- The base query returns many rows with no aggregation (just use an index)

### 4.2 REFRESH MATERIALIZED VIEW CONCURRENTLY

**Requirements:**
- The materialized view must have a **UNIQUE index** (or it falls back to non-concurrent refresh)
- The UNIQUE index must cover all rows (no partial unique index)

**Lock behavior:**
- Regular refresh: **ACCESS EXCLUSIVE** (blocks all reads)
- Concurrent refresh: **EXCLUSIVE** (blocks writes to the matview, allows reads)
- Concurrent refresh works by building a new copy and diffing against the existing data

```sql
-- Create matview with required unique index
CREATE MATERIALIZED VIEW mv_daily_sales AS
SELECT
  date_trunc('day', order_date) AS sale_date,
  product_id,
  sum(quantity) AS total_qty,
  sum(amount) AS total_amount
FROM orders
GROUP BY 1, 2;

-- Required for CONCURRENTLY
CREATE UNIQUE INDEX idx_mv_daily_sales
  ON mv_daily_sales (sale_date, product_id);

-- Non-blocking refresh (allows reads during refresh)
REFRESH MATERIALIZED VIEW CONCURRENTLY mv_daily_sales;
```

### 4.3 Staleness Detection

PostgreSQL has **no built-in matview staleness tracking**. pg_sage must implement its own:

```sql
-- Create a tracking table for matview refresh history
CREATE TABLE IF NOT EXISTS pgsage_matview_refresh_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  matview_name text NOT NULL,
  refresh_started_at timestamptz NOT NULL,
  refresh_completed_at timestamptz,
  refresh_duration interval GENERATED ALWAYS AS
    (refresh_completed_at - refresh_started_at) STORED,
  rows_before bigint,
  rows_after bigint,
  success boolean DEFAULT false
);

-- Staleness query
SELECT
  m.matviewname,
  r.refresh_completed_at AS last_refresh,
  now() - r.refresh_completed_at AS staleness,
  pg_size_pretty(pg_total_relation_size(
    (m.schemaname || '.' || m.matviewname)::regclass
  )) AS matview_size
FROM pg_matviews m
LEFT JOIN LATERAL (
  SELECT refresh_completed_at
  FROM pgsage_matview_refresh_log
  WHERE matview_name = m.schemaname || '.' || m.matviewname
    AND success = true
  ORDER BY refresh_completed_at DESC
  LIMIT 1
) r ON true
WHERE m.schemaname NOT IN ('pg_catalog', 'information_schema');
```

**Staleness heuristics for pg_sage:**
- Track change rate on source tables via `pg_stat_user_tables.n_tup_ins + n_tup_upd + n_tup_del`
- If source table changes > X% since last refresh, recommend refresh
- If matview hasn't been refreshed in > configurable threshold, alert
- Track refresh duration over time to predict maintenance windows

### 4.4 Disk Space Impact Estimation

```sql
-- Estimate matview size from source query before creation
-- Use EXPLAIN to estimate rows, multiply by avg row width
EXPLAIN (FORMAT JSON)
SELECT date_trunc('day', order_date) AS d, product_id, sum(quantity), sum(amount)
FROM orders GROUP BY 1, 2;

-- After creation, compare sizes
SELECT
  'source' AS relation,
  pg_size_pretty(pg_total_relation_size('orders')) AS size
UNION ALL
SELECT
  'matview',
  pg_size_pretty(pg_total_relation_size('mv_daily_sales'));
```

**Rule of thumb for pg_sage recommendations:**
- Aggregation matviews: typically 1-10% of source table size
- JOIN matviews: can be 50-200% of largest source table
- Always estimate before recommending; reject if > 20% of available disk

### 4.5 pg_ivm (Incremental View Maintenance)

**Status as of 2026:** pg_ivm 1.12 released. **Not production-ready** for most deployments.

**How it works:**
- Uses AFTER triggers to incrementally apply changes to materialized views
- Only computes deltas, not full refreshes
- Blocks writes to source table until matview update completes

**Limitations:**
- Stability concerns for production workloads
- Write performance impact from trigger-based synchronous updates
- Not available on most managed PostgreSQL services (RDS, Cloud SQL, etc.)
- Limited query support (not all aggregations/JOINs supported)

**pg_sage recommendation:** Monitor pg_ivm development; do not recommend for production use until stability issues are resolved. Use scheduled `REFRESH MATERIALIZED VIEW CONCURRENTLY` instead.

### 4.6 TimescaleDB Continuous Aggregates

**If TimescaleDB is detected**, continuous aggregates are superior to standard matviews for time-series data:

| Feature | Matview | Continuous Aggregate |
|---|---|---|
| Refresh | Manual/scheduled | Automatic (policy-based) |
| Update scope | Full recompute or diff | Incremental (changed buckets only) |
| Real-time query | Stale until refresh | Combines materialized + live data |
| Lock during refresh | ACCESS EXCLUSIVE or EXCLUSIVE | None (background worker) |
| Requirement | Standard PostgreSQL | TimescaleDB extension |

**Real-time aggregation:** By default, querying a continuous aggregate transparently combines pre-computed results with a live aggregation of data newer than the materialization watermark. This gives near-real-time accuracy with pre-computed performance.

**Performance:** ~18ms for continuous aggregate query vs ~5ms for fully materialized (and stale) matview vs ~15s for raw view — 1000x faster than raw with near-real-time freshness.

```sql
-- TimescaleDB continuous aggregate example
CREATE MATERIALIZED VIEW daily_device_stats
WITH (timescaledb.continuous) AS
SELECT
  time_bucket('1 day', ts) AS bucket,
  device_id,
  avg(temperature) AS avg_temp,
  max(temperature) AS max_temp
FROM sensor_data
GROUP BY 1, 2;

-- Auto-refresh policy
SELECT add_continuous_aggregate_policy('daily_device_stats',
  start_offset => INTERVAL '3 days',
  end_offset   => INTERVAL '1 hour',
  schedule_interval => INTERVAL '1 hour');
```

---

## 5. PostgreSQL-Specific Migration Patterns

### 5.1 NOT VALID + VALIDATE CONSTRAINT Two-Step

The most important safe migration pattern. Works for CHECK constraints and FOREIGN KEY constraints.

```sql
-- Step 1: Add constraint WITHOUT validation (brief lock, no scan)
BEGIN;
  SET LOCAL lock_timeout = '2s';

  -- For CHECK constraints:
  ALTER TABLE orders
    ADD CONSTRAINT chk_orders_positive_amount
    CHECK (amount > 0) NOT VALID;
  -- Takes ACCESS EXCLUSIVE but completes instantly (no table scan)

  -- For FOREIGN KEY constraints:
  ALTER TABLE orders
    ADD CONSTRAINT fk_orders_customer
    FOREIGN KEY (customer_id) REFERENCES customers(id) NOT VALID;
  -- Takes SHARE ROW EXCLUSIVE briefly, skips validation scan
COMMIT;

-- Step 2: Validate in separate transaction (allows concurrent writes)
ALTER TABLE orders VALIDATE CONSTRAINT chk_orders_positive_amount;
-- Takes SHARE UPDATE EXCLUSIVE — reads AND writes continue
-- Scans entire table to verify existing rows meet constraint

ALTER TABLE orders VALIDATE CONSTRAINT fk_orders_customer;
-- Same: SHARE UPDATE EXCLUSIVE, allows concurrent DML
```

**Key details:**
- NOT VALID constraints still enforce on **new** INSERT/UPDATE operations immediately
- Only the validation of **existing** data is deferred
- VALIDATE CONSTRAINT takes **SHARE UPDATE EXCLUSIVE** (allows reads and writes)
- If validation fails (bad data exists), the constraint stays NOT VALID

### 5.2 Adding NOT NULL Safely (PG12+)

Direct `SET NOT NULL` acquires ACCESS EXCLUSIVE and scans the entire table. The safe alternative:

```sql
-- Step 1: Add a CHECK constraint (NOT VALID to avoid scan)
ALTER TABLE orders
  ADD CONSTRAINT chk_orders_name_not_null
  CHECK (name IS NOT NULL) NOT VALID;

-- Step 2: Validate the constraint (allows concurrent writes)
ALTER TABLE orders VALIDATE CONSTRAINT chk_orders_name_not_null;

-- Step 3: Set NOT NULL (PG12+ recognizes the validated CHECK, skips scan)
ALTER TABLE orders ALTER COLUMN name SET NOT NULL;
-- PG12+: recognizes existing validated CHECK (IS NOT NULL) and skips scan
-- Still ACCESS EXCLUSIVE but instant because scan is unnecessary

-- Step 4: Drop redundant CHECK constraint
ALTER TABLE orders DROP CONSTRAINT chk_orders_name_not_null;
```

**PG version note:** In PG11 and earlier, SET NOT NULL always performs a full table scan even with a matching CHECK constraint. PG12+ optimizes this by checking for an existing validated NOT NULL CHECK.

### 5.3 Adding Columns with Defaults (PG11+ Optimization)

**PG11+ fast path:** Adding a column with a **non-volatile** DEFAULT is instant, regardless of table size. The default is stored in `pg_attribute.attmissingval` and applied on read.

```sql
-- FAST (PG11+): non-volatile default — instant, no rewrite
ALTER TABLE orders ADD COLUMN status text NOT NULL DEFAULT 'pending';
-- Stored in catalog, existing rows read the default from attmissingval

-- FAST: immutable function default
ALTER TABLE orders ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
-- now() is STABLE, treated as non-volatile for this purpose

-- SLOW: volatile function default — triggers full table rewrite
ALTER TABLE orders ADD COLUMN random_id double precision DEFAULT random();
-- random() is VOLATILE — every row needs a different value → rewrite
```

**How to check if a function is volatile:**

```sql
SELECT proname, provolatile
FROM pg_proc
WHERE proname = 'your_function_name';
-- 'i' = immutable, 's' = stable, 'v' = volatile
```

### 5.4 Renaming Columns Safely

`ALTER TABLE ... RENAME COLUMN` acquires ACCESS EXCLUSIVE but is instant (metadata-only). The danger is application-level: any running query using the old name will fail.

**Expand-contract pattern for zero-downtime rename:**

```sql
-- Phase 1 (Expand): Add new column, keep old
ALTER TABLE orders ADD COLUMN customer_name text;

-- Phase 2: Dual-write with trigger
CREATE OR REPLACE FUNCTION sync_customer_name() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'INSERT' OR TG_OP = 'UPDATE' THEN
    IF NEW.customer_name IS NULL AND NEW.cust_name IS NOT NULL THEN
      NEW.customer_name := NEW.cust_name;
    ELSIF NEW.cust_name IS NULL AND NEW.customer_name IS NOT NULL THEN
      NEW.cust_name := NEW.customer_name;
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_sync_customer_name
  BEFORE INSERT OR UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION sync_customer_name();

-- Phase 3: Backfill existing rows (in batches)
UPDATE orders SET customer_name = cust_name
WHERE customer_name IS NULL AND ctid = ANY(
  ARRAY(SELECT ctid FROM orders WHERE customer_name IS NULL LIMIT 5000)
);

-- Phase 4 (Contract): After all code uses new column
ALTER TABLE orders DROP COLUMN cust_name;
DROP TRIGGER trg_sync_customer_name ON orders;
DROP FUNCTION sync_customer_name();
```

**Simpler alternative using views:**

```sql
-- Rename table, create view with old name
BEGIN;
  ALTER TABLE orders RENAME COLUMN cust_name TO customer_name;
  -- Create view for backward compatibility
  CREATE VIEW orders_compat AS
    SELECT *, customer_name AS cust_name FROM orders;
COMMIT;
-- Updatable view allows writes through the old name
-- Drop view after all code is updated
```

### 5.5 Splitting Wide Tables

**Pattern: Extract columns to a new table without downtime**

```sql
-- Phase 1: Create new table
CREATE TABLE order_details (
  order_id bigint PRIMARY KEY REFERENCES orders(id),
  description text,
  internal_notes text,
  metadata jsonb
);

-- Phase 2: Trigger for dual-write
CREATE OR REPLACE FUNCTION sync_order_details() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    INSERT INTO order_details (order_id, description, internal_notes, metadata)
    VALUES (NEW.id, NEW.description, NEW.internal_notes, NEW.metadata)
    ON CONFLICT (order_id) DO UPDATE SET
      description = EXCLUDED.description,
      internal_notes = EXCLUDED.internal_notes,
      metadata = EXCLUDED.metadata;
  ELSIF TG_OP = 'UPDATE' THEN
    INSERT INTO order_details (order_id, description, internal_notes, metadata)
    VALUES (NEW.id, NEW.description, NEW.internal_notes, NEW.metadata)
    ON CONFLICT (order_id) DO UPDATE SET
      description = EXCLUDED.description,
      internal_notes = EXCLUDED.internal_notes,
      metadata = EXCLUDED.metadata;
  ELSIF TG_OP = 'DELETE' THEN
    DELETE FROM order_details WHERE order_id = OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_sync_order_details
  AFTER INSERT OR UPDATE OR DELETE ON orders
  FOR EACH ROW EXECUTE FUNCTION sync_order_details();

-- Phase 3: Backfill (batched)
INSERT INTO order_details (order_id, description, internal_notes, metadata)
SELECT id, description, internal_notes, metadata
FROM orders
WHERE id > $last_processed_id  -- process in batches of 10000
ORDER BY id
LIMIT 10000
ON CONFLICT (order_id) DO NOTHING;

-- Phase 4: Update application to read from order_details
-- Phase 5: Drop old columns and trigger
ALTER TABLE orders DROP COLUMN description;
ALTER TABLE orders DROP COLUMN internal_notes;
ALTER TABLE orders DROP COLUMN metadata;
DROP TRIGGER trg_sync_order_details ON orders;
DROP FUNCTION sync_order_details();
```

### 5.6 Type Changes — Safe vs Unsafe Casts

**No rewrite required (binary coercible):**

| From | To | Notes |
|---|---|---|
| `varchar(n)` | `text` | Same binary representation |
| `varchar(n)` | `varchar(m)` where m > n | Increasing length, no rewrite |
| `text` | `varchar` (unbounded) | Same representation |
| `varchar(n)` | `varchar` (unbounded) | Removing length constraint |
| `cidr` | `inet` | Binary compatible |

**Rewrite required (different binary representation):**

| From | To | Notes |
|---|---|---|
| `integer` | `bigint` | 4 bytes → 8 bytes |
| `smallint` | `integer` | 2 bytes → 4 bytes |
| `timestamp` | `timestamptz` | Different internal representation |
| `numeric(p,s)` | `numeric(p2,s2)` | If precision changes |
| `text` | `integer` | Completely different type |

**Safe migration for integer → bigint (without table rewrite):**

```sql
-- Step 1: Add new bigint column
ALTER TABLE orders ADD COLUMN id_new bigint;

-- Step 2: Create trigger for dual-write
CREATE OR REPLACE FUNCTION sync_id_bigint() RETURNS trigger AS $$
BEGIN
  NEW.id_new := NEW.id;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_sync_id
  BEFORE INSERT OR UPDATE ON orders
  FOR EACH ROW EXECUTE FUNCTION sync_id_bigint();

-- Step 3: Backfill in batches
UPDATE orders SET id_new = id
WHERE id_new IS NULL AND ctid = ANY(
  ARRAY(SELECT ctid FROM orders WHERE id_new IS NULL LIMIT 10000)
);

-- Step 4: Create new sequence
CREATE SEQUENCE orders_id_new_seq AS bigint;
SELECT setval('orders_id_new_seq', max(id_new)) FROM orders;
ALTER TABLE orders ALTER COLUMN id_new SET DEFAULT nextval('orders_id_new_seq');

-- Step 5: Swap (requires brief ACCESS EXCLUSIVE)
BEGIN;
  SET LOCAL lock_timeout = '5s';
  LOCK TABLE orders IN ACCESS EXCLUSIVE MODE;
  ALTER TABLE orders DROP CONSTRAINT orders_pkey;
  ALTER TABLE orders RENAME COLUMN id TO id_old;
  ALTER TABLE orders RENAME COLUMN id_new TO id;
  ALTER TABLE orders ADD PRIMARY KEY (id);
  -- Update FKs in referencing tables similarly
COMMIT;

-- Step 6: Cleanup
ALTER TABLE orders DROP COLUMN id_old;
DROP TRIGGER trg_sync_id ON orders;
DROP FUNCTION sync_id_bigint();
```

### 5.7 Extension Upgrades

```sql
-- Check current extension versions
SELECT extname, extversion, extrelocatable
FROM pg_extension
ORDER BY extname;

-- Check available upgrade targets
SELECT name, default_version, installed_version
FROM pg_available_extensions
WHERE installed_version IS NOT NULL
  AND installed_version <> default_version;

-- Upgrade an extension
ALTER EXTENSION pg_stat_statements UPDATE;  -- to latest
ALTER EXTENSION postgis UPDATE TO '3.4.0';  -- to specific version

-- Multi-step upgrades (if no direct path exists)
ALTER EXTENSION postgis UPDATE TO '3.2.0';
ALTER EXTENSION postgis UPDATE TO '3.4.0';
```

**Key points:**
- New extension files must be on the filesystem **before** running ALTER EXTENSION
- Update scripts follow the pattern `extension--old--new.sql`
- Usually no restart required — changes apply within the session
- Always test upgrades on a replica first
- Some extensions (e.g., PostGIS) have their own upgrade functions

---

## 6. Version-Specific DDL Improvements

### PG12 (2019)

- **REINDEX CONCURRENTLY**: Rebuild indexes without blocking writes
  ```sql
  REINDEX INDEX CONCURRENTLY idx_orders_date;
  REINDEX TABLE CONCURRENTLY orders;
  ```
- **SET NOT NULL optimization**: Recognizes existing validated CHECK (IS NOT NULL) constraints, skips full table scan
- **Generated columns (STORED)**: Computed columns maintained automatically
- **CTE inlining**: CTEs can now be inlined by the optimizer (performance improvement, not DDL)

### PG13 (2020)

- **DROP DATABASE FORCE**: Can drop a database even with active connections
  ```sql
  DROP DATABASE mydb WITH (FORCE);
  ```
- **Parallel VACUUM indexes**: VACUUM can process indexes in parallel
- **Incremental sorting**: Leverages existing sort order from indexes
- **Improved partitioning**: Better query planning for partitioned tables with many partitions

### PG14 (2021)

- **DETACH PARTITION CONCURRENTLY**: Non-blocking partition detach
  ```sql
  ALTER TABLE measurements DETACH PARTITION measurements_old CONCURRENTLY;
  ```
- **Multirange types**: New range type for sets of ranges
- **pg_stat_statements**: Added `toplevel` column to distinguish top-level vs nested calls
- **Compression for TOAST**: LZ4 compression option for TOAST data
  ```sql
  ALTER TABLE orders ALTER COLUMN description SET COMPRESSION lz4;
  ```
- **Subscript syntax for JSONB**: `jsonb_col['key']` instead of `jsonb_col->'key'`

### PG15 (2022)

- **SET ACCESS METHOD**: Change table access method (requires rewrite)
  ```sql
  ALTER TABLE orders SET ACCESS METHOD columnar;  -- with extensions
  ```
- **MERGE statement**: SQL-standard UPSERT alternative
- **Logical replication improvements**: Row filtering and column lists
  ```sql
  CREATE PUBLICATION pub FOR TABLE orders (id, amount, status)
    WHERE (status = 'completed');
  ```
- **Public schema permissions**: CREATE privilege on public schema revoked by default
- **pg_stat_statements**: Track JIT compilation stats
- **Security invoker views**: Views that check permissions of the calling user
  ```sql
  CREATE VIEW v WITH (security_invoker = true) AS SELECT ...;
  ```

### PG16 (2023)

- **Logical replication from standby**: Replicate from read replicas
- **Improved COPY performance**: Parallel COPY for loading data
- **pg_stat_io**: New view for I/O statistics
- **ANY_VALUE aggregate**: Returns an arbitrary value from the group
- **Right JOIN and anti-join optimization**: Better join planning

### PG17 (2024)

- **Incremental backup**: Built-in incremental backup with `pg_basebackup --incremental`
- **Improved VACUUM**: More efficient handling of frozen pages
- **JSON_TABLE**: SQL/JSON table function for shredding JSON into rows
  ```sql
  SELECT * FROM JSON_TABLE(
    '{"orders": [{"id": 1}, {"id": 2}]}',
    '$.orders[*]' COLUMNS (order_id int PATH '$.id')
  );
  ```
- **Identity column improvements**: Better handling of identity columns during logical replication
- **Partition pruning improvements**: Better runtime partition pruning
- **MERGE improvements**: RETURNING clause for MERGE
- **pg_stat_checkpointer**: Replaces pg_stat_bgwriter checkpoint stats

### PG18 (2025, latest)

- **Virtual generated columns**: Computed on read, no storage
  ```sql
  ALTER TABLE orders ADD COLUMN full_name text
    GENERATED ALWAYS AS (first_name || ' ' || last_name) VIRTUAL;
  -- No table rewrite, no storage overhead
  ```
- **NOT NULL constraint improvements**: `NOT NULL NOT ENFORCED` option
- **MAINTAIN privilege**: Grant refresh permission on matviews without full ownership
  ```sql
  GRANT MAINTAIN ON MATERIALIZED VIEW mv_sales TO app_user;
  ```
- **Improved EXPLAIN output**: Better formatting and more detail
- **UUIDv7 generation**: `uuidv7()` function for time-ordered UUIDs

---

## Appendix A: pg_sage Implementation Checklist

### DDL Safety Checks (before recommending any DDL)

1. Check `pg_stat_activity` for long-running transactions on target table
2. Check `pg_locks` for existing lock contention
3. Determine lock level required for the proposed operation
4. If ACCESS EXCLUSIVE needed: recommend `SET lock_timeout` + retry pattern
5. If table rewrite needed: estimate duration from table size and I/O throughput
6. For large tables (> 1GB): always recommend the safe alternative pattern
7. Never recommend DDL during peak traffic hours without lock_timeout

### Schema Review Queries (run periodically)

| Check | Priority | Query Section |
|---|---|---|
| Missing FK indexes | Critical | 2.1 |
| Integer sequence exhaustion (>50%) | Critical | 2.2 |
| Tables without primary keys | High | 2.6 |
| Unused indexes (0 scans, >1MB) | High | 2.4 |
| Duplicate/overlapping indexes | High | 2.5 |
| timestamp without timezone | Medium | 2.2 |
| serial instead of identity | Medium | 2.3 |
| TOAST-heavy tables (>50% TOAST) | Medium | 2.7 |
| Nullable columns with 0% nulls | Low | 2.10 |
| JSONB-heavy tables | Low | 2.8 |

### N+1 Detection Pipeline

1. Snapshot `pg_stat_statements` every 60 seconds (delta computation)
2. Flag queries where: `delta_calls > 100 AND mean_exec_time < 5ms AND rows_per_call <= 1`
3. Cross-reference with `pg_stat_activity` sampling for temporal correlation
4. Filter false positives (health checks, monitoring, cached lookups)
5. Generate recommendation: batch query or JOIN alternative
6. Track whether recommendation was acted on (calls should decrease)

### Matview Intelligence Pipeline

1. Detect existing matviews and their refresh patterns
2. Track source table change rate via `pg_stat_user_tables` deltas
3. Estimate staleness based on last refresh vs source changes
4. Monitor refresh duration trends
5. Alert if refresh duration is growing (suggests need for optimization)
6. Recommend CONCURRENTLY refresh when unique index exists
7. Detect matviews without unique indexes (cannot use CONCURRENTLY)

---

## Sources

- [PostgreSQL Documentation: Explicit Locking](https://www.postgresql.org/docs/current/explicit-locking.html)
- [PostgreSQL Documentation: ALTER TABLE](https://www.postgresql.org/docs/current/sql-altertable.html)
- [Citus Data: 7 Tips for Dealing with Postgres Locks](https://www.citusdata.com/blog/2018/02/22/seven-tips-for-dealing-with-postgres-locks/)
- [PostgresAI: Zero-Downtime Migrations with lock_timeout and Retries](https://postgres.ai/blog/20210923-zero-downtime-postgres-schema-migrations-lock-timeout-and-retries)
- [Xata: Schema Changes and the Postgres Lock Queue](https://xata.io/blog/migrations-and-exclusive-locks)
- [PostgreSQL Wiki: Don't Do This](https://wiki.postgresql.org/wiki/Don't_Do_This)
- [PostgreSQL Wiki: Index Maintenance](https://wiki.postgresql.org/wiki/Index_Maintenance)
- [PostgreSQL Wiki: Incremental View Maintenance](https://wiki.postgresql.org/wiki/Incremental_View_Maintenance)
- [Brandur: Fast Column Creation with Defaults (PG11)](https://brandur.org/postgres-default)
- [Crunchy Data: Serials Should be BIGINT](https://www.crunchydata.com/blog/postgres-serials-should-be-bigint-and-how-to-migrate)
- [Crunchy Data: Integer Overflow in Postgres](https://www.crunchydata.com/blog/the-integer-at-the-end-of-the-universe-integer-overflow-in-postgres)
- [CYBERTEC: Lookup Table or Enum Type?](https://www.cybertec-postgresql.com/en/lookup-table-or-enum-type/)
- [CYBERTEC: Get Rid of Unused Indexes](https://www.cybertec-postgresql.com/en/get-rid-of-your-unused-indexes/)
- [Squawk: Adding Foreign Key Constraint](https://squawkhq.com/docs/adding-foreign-key-constraint)
- [Squawk: Changing Column Type](https://squawkhq.com/docs/changing-column-type)
- [Haki Benita: Medium-Size Texts Performance](https://hakibenita.com/sql-medium-text-performance)
- [Heap.io: When To Avoid JSONB](https://www.heap.io/blog/when-to-avoid-jsonb-in-a-postgresql-schema)
- [Percona: pg_stat_monitor](https://docs.percona.com/pg-stat-monitor/)
- [Timescale: Continuous Aggregates](https://www.timescale.com/blog/how-postgresql-views-and-materialized-views-work-and-how-they-influenced-timescaledb-continuous-aggregates/)
- [pg_ivm GitHub](https://github.com/sraoss/pg_ivm)
- [Percona: PostgreSQL Extension Upgrades](https://www.percona.com/blog/upgrading-postgresql-extensions/)
- [Brandur: Postgres Table Rename with Views](https://brandur.org/fragments/postgres-table-rename)
- [PostgresAI: Invalid Index Overhead](https://postgres.ai/blog/20260106-invalid-index-overhead)
