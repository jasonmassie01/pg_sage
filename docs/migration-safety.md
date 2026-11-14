# Migration safety: online column type changes

`ALTER TABLE ... ALTER COLUMN ... TYPE` rewrites the whole table while it holds ACCESS
EXCLUSIVE, so nothing can read or write the table until it finishes. When pg_sage's schema
guard finds a type tightening (a text column whose values are all integers, for example),
it records the in-place `ALTER TYPE` only as reference text, labelled as a rewrite, and next
to it an **online column swap script** that makes the same change without the rewrite.

The script is **recommend-only**. pg_sage generates it from the catalog and never runs it,
proposes it for approval, or routes it to the executor. You run it yourself, step by step,
after review.

The other online rewrite pg_sage plans, `apply_migration` for `ADD CONSTRAINT ... UNIQUE`
and `SET NOT NULL`, is described with the forms it supports and the ones it refuses in
[what `apply_migration` can plan](mcp.md#what-apply_migration-can-plan). Both planners
need a build with the SQL parser (cgo): without it `apply_migration` refuses to plan, and
the column swap below is marked degraded.

## Where to find it

A type-tightening decision in `sage.decision` (feature `schema_guard`) carries:

| Evidence key | What it holds |
|---|---|
| `rewrite_statement` | the in-place `ALTER TYPE` (a full rewrite; shown for reference, not to run) |
| `online_swap_script` | the online swap script described below |
| `online_swap_note` | the recommend-only label |
| `online_swap_refused` | instead of a script: why this column cannot be swapped online |

## What the script does

Every statement is preceded by a comment with its step name and the lock it takes. The lock
levels are the planner's; on a build with the SQL parser each DDL statement is also run
through the migration classifier (the same one `lint_migration` uses), and a plan whose
statement the classifier does not know, or that it says takes a stronger lock, is refused.
The backfill `UPDATE` is DML, which the DDL classifier does not classify; it takes ROW
EXCLUSIVE and row locks on one batch at a time.

1. **prepare**: `SET lock_timeout` (2 s by default) and `SET plan_cache_mode =
   force_custom_plan` for the session; add the new column (`<col>__swap_new`, nullable, no
   default: a catalog-only change); create a trigger function and a `BEFORE INSERT OR
   UPDATE` trigger that copies every write into the new column; for a NOT NULL column, add
   `CHECK (<col>__swap_new IS NOT NULL) NOT VALID`.
2. **backfill**: one keyset batch per statement, walking the primary key. Run it with
   `$1` = NULL, then with `$1` = the largest key the last batch returned, until a batch
   returns no rows. Each batch is its own transaction.
3. **verify**: `VALIDATE CONSTRAINT` the NOT NULL proof (SHARE UPDATE EXCLUSIVE); for a
   primary key column, `CREATE UNIQUE INDEX CONCURRENTLY` on the new column.
4. **swap**: one short transaction. `LOCK TABLE` the referencing tables, then the table, in
   ACCESS EXCLUSIVE; drop the referencing foreign keys and the primary key; drop the
   trigger; free the old column (drop its default and NOT NULL); swap the names; `SET NOT
   NULL` (no scan: the validated CHECK proves it); attach the primary key `USING INDEX`;
   move the default and the owned sequence (`ALTER SEQUENCE ... AS <type> OWNED BY`); re-add
   each referencing foreign key `NOT VALID` with its actions and timing. Every statement in
   it is metadata-only.
5. **finish**: `VALIDATE CONSTRAINT` each re-added foreign key; drop the trigger function.
6. **contract** (destructive): drop the old column (`<col>__swap_old`), once nothing reads it.
   Until then it holds the pre-swap values, so the change can be undone by hand.

Run the whole script in **one session**: the `SET` statements apply to that session only.

## Running it safely

- A statement that waits for a lock fails with `lock_timeout` (SQLSTATE 55P03) instead of
  queueing every other query behind it. Run that step again; for the swap transaction,
  `ROLLBACK` and run it again from `BEGIN`.
- If `CREATE UNIQUE INDEX CONCURRENTLY` fails it leaves an invalid index: drop it with
  `DROP INDEX CONCURRENTLY` and run the step again.
- When the conversion can fail (text to bigint, for example), the trigger makes a write whose
  value does not convert fail from the prepare step on, as it would after the change. The
  trigger step's comment says so.
- Clients that cache prepared statements returning the column may get `cached plan must not
  change result type` once after the swap; they recover on retry or reconnect.
- The new column is a new attribute: re-apply any column-level `GRANT`, `COMMENT ON COLUMN`,
  collation or `SET STATISTICS` the old column had, and expect it last in `SELECT *` order.
- Other `BEFORE` row triggers fire in name order with the sync trigger; one that changes the
  column after the sync trigger has run leaves the new column stale. Check for them first.

## What is refused

The planner refuses, with a reason, anything it cannot carry over exactly:

- a table without a primary key, or with a multi-column one (the backfill walks a single key);
- a partitioned table, a partition, or a table in an inheritance tree;
- a key referenced by a foreign key from a partitioned table or a partition (out of scope);
- multi-column foreign keys, `ON DELETE SET NULL (cols)`, and foreign keys on the column itself;
- a referencing foreign key that is `NOT VALID` or (PostgreSQL 18) `NOT ENFORCED`: the swap
  would re-add it enforced and validate it;
- a primary key the swap moves that is `DEFERRABLE`, has `INCLUDE` columns, or is the
  table's replica identity index: the rebuilt key would differ;
- a referencing column outside the integer family when the key changes type;
- identity and generated columns, a default other than the column's own sequence, and a
  sequence wider than the new type, or a column that owns more than one sequence;
- a column other objects depend on (indexes other than the primary key's, views, CHECK
  constraints, triggers with column lists, statistics, policies), which would be lost with
  the old column;
- target types other than `smallint`, `integer`, `bigint`, `numeric`, `text`, `uuid`, `date`,
  `boolean`, `jsonb`, `timestamp with time zone` and `timestamp without time zone`;
- derived names longer than 63 bytes (PostgreSQL would truncate them).

The catalog reads behind the plan run in pg_sage's bounded read-only transactions, with the
shipped `safety.query_timeout_ms` default and its lock timeout. They run only when the schema
guard records a changed type-tightening decision.

## Verified behaviour

The integration test `TestColumnSwapOnlineUnderWrites` runs the generated script for a
1M-row `integer` primary key, filled from its sequence and referenced by a foreign key with
`ON DELETE CASCADE`, while three writers insert and update both tables. It checks that no
lock-only step and no swap transaction attempt runs past `lock_timeout`, that the writers are
never blocked for more than about twice the `lock_timeout`, that every pre-swap value
reached the new column, and that afterwards the column is a `bigint NOT NULL` primary key
filled from a `bigint` sequence, with the foreign key validated and enforced and nothing of
the swap left behind. It runs on PostgreSQL 14 and 17.
