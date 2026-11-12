# Migration review in CI

pg_sage reviews a migration **before it merges**, with the same engine its migration
advisor uses on DDL that is already running. One review, three ways in:

| Entry point | Who it is for | Auth |
|---|---|---|
| `pg_sage lint-migration` | CI pipelines (GitHub Actions, any runner) | A read-only DSN in `SAGE_LINT_DATABASE_URL`, or none with `--static` |
| `POST /api/v1/migrations/lint` | Tools that already talk to the pg_sage API | A signed-in session (any role) |
| MCP `lint_migration` | Coding agents | An MCP token ([MCP](mcp.md)) |

The SQL is text to analyze: **it is never run**. pg_sage reads only catalog statistics
(table size and rows, active queries and lock waiters on the table, replication lag, a
column's current type) under a 2 s `statement_timeout`.

## What a review says

Each statement gets the DDL safety rules it matches, the lock it takes, whether it
rewrites the table, the table's live rows and bytes, a safer alternative, and a
deterministic **guarded rewrite**:

* `SET lock_timeout = '5s';` before a statement that takes ACCESS EXCLUSIVE when no
  earlier statement set it (the advisor's annotation; it is advice, not a scored hazard);
* `CREATE INDEX CONCURRENTLY`, `REINDEX ... CONCURRENTLY`, `DROP INDEX CONCURRENTLY`,
  `REFRESH MATERIALIZED VIEW CONCURRENTLY`, `DETACH PARTITION ... CONCURRENTLY`;
* `ADD CONSTRAINT ... NOT VALID` followed by `VALIDATE CONSTRAINT`;
* `SET NOT NULL` through a validated `CHECK (col IS NOT NULL)`;
* `ADD COLUMN ... DEFAULT volatile()` split into a plain `ADD COLUMN`, `SET DEFAULT`,
  and a batched backfill.

A statement that holds a string literal gets no rewrite (lint redacts literals, and a
rewrite with a redacted literal would not run); apply the safe alternative by hand.

### Verdicts and exit codes

| Verdict | Exit | Meaning |
|---|---|---|
| `safe` | 0 | Every statement is known and none locks or rewrites a table |
| `review` | 1 | A rule matched (any lock or rewrite is at least review), or a statement is unrecognized or does not parse: unrecognized DDL is not proven safe |
| `risky` | 2 | A risk score of 0.7 or more, or a table of at least `migration.ddl_row_threshold` rows (default 10000) that the statement blocks for as long as the table is big: a scan or rewrite under SHARE or stronger, or an ACCESS EXCLUSIVE change with no `lock_timeout` |
| (tool error) | 3 | Unreadable, empty or over-100 kB file, no connection, a catalog read that failed or timed out |

Exit 3 is never a verdict: a gate that cannot measure the migration fails, it does not
read as "review". Below the row threshold a metadata-only ACCESS EXCLUSIVE change is
review, not risky.

### Static mode

`--static` (and `"static": true` on the endpoint) reads no database. Sizes show as
`unknown`, only the rules' intrinsic hazards are scored, and ACCESS EXCLUSIVE or a
rewrite is never below review. Give `--pg-version` for version-dependent rules (for
example a volatile `DEFAULT` on PostgreSQL 14-18); without it, a static review uses the
oldest supported major, 14, so no rule that bites on older servers is missed.

## The CLI

```text
pg_sage lint-migration --file x.sql [--static] [--pg-version 14-18]
    [--row-threshold N] [--format json|github|sarif]
```

* The DSN comes from `SAGE_LINT_DATABASE_URL` (or a file named by
  `SAGE_LINT_DATABASE_URL_FILE`). Point it at a replica or a staging copy with
  production-sized tables, as a role that can read `pg_class`, `pg_stat_activity`,
  `pg_locks` and `pg_stat_replication`; `pg_monitor` is enough. It opens one connection.
* `--format json` (default) prints the full report; `github` prints
  [workflow annotations](https://docs.github.com/actions/using-workflows/workflow-commands-for-github-actions)
  on the statement's line (risky is an error, review a warning); `sarif` prints SARIF
  2.1.0 for code scanning.
* `--row-threshold` is `migration.ddl_row_threshold` (default 10000).

### GitHub Actions

```yaml
name: Migration review
on:
  pull_request:
    paths: ["migrations/**.sql"]
permissions:
  contents: read
jobs:
  lint-migrations:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Install pg_sage
        run: |
          curl -fsSL -o pg_sage.tar.gz \
            https://github.com/jasonmassie01/pg_sage/releases/latest/download/pg_sage_linux_amd64.tar.gz
          tar -xzf pg_sage.tar.gz pg_sage
      - name: Review changed migrations
        env:
          SAGE_LINT_DATABASE_URL: ${{ secrets.STAGING_READONLY_DSN }}
        run: |
          status=0
          for f in $(git diff --name-only origin/${{ github.base_ref }}... -- 'migrations/*.sql'); do
            ./pg_sage lint-migration --file "$f" --format github || rc=$?
            rc=${rc:-0}
            # 3 is a tool error: fail. 2 is risky: fail. 1 is review: annotate only.
            if [ "$rc" -ge 2 ]; then status=1; fi
            unset rc
          done
          exit $status
```

Without a staging DSN, drop the `env:` block and pass `--static --pg-version 17`. To
upload SARIF instead, run with `--format sarif > lint.sarif` and add
`github/codeql-action/upload-sarif` with `sarif_file: lint.sarif`.

pg_sage's own CI runs this gate against `test-fixtures/migrations` (one safe, one
review, one risky migration), statically and against its test PostgreSQL, with
`scripts/ci/lint-migration-fixtures.sh`.

## The endpoint

```http
POST /api/v1/migrations/lint
Content-Type: application/json

{"sql": "CREATE INDEX x ON app.t (c)", "database": "orders", "pg_version": 0, "static": false}
```

* Any signed-in role may call it (it changes nothing). The response is the report
  (`verdict`, `max_risk_score`, `statements[]` with `hazards`, `guarded_rewrite`, `line`).
* 400: empty SQL, SQL over 100,000 bytes, a body that is not one JSON object, or an
  unsupported `pg_version`. 404: an unknown `database`. 429 with `Retry-After`: more than
  `api.migration_lint_rate_per_minute` (default 30) requests from one user in a minute.
  503 with the reason: no connected database, or a catalog read that failed or hit the
  2 s timeout; never a verdict.
* In fleet mode `database` selects the instance; with one monitored database it may be
  left out. At most 4 reviews read catalogs at once, so a burst cannot take the monitored
  database's pool.

API-token access (without a session) is not offered yet: CI should use the CLI, which
needs only a read-only DSN.

## Managed providers

The review needs only catalog reads, so it works on RDS, Aurora, Cloud SQL, AlloyDB,
Azure and Lakebase with a read-only role. PostgreSQL 14-18 are covered by the
version-aware rules.

## One engine

The CLI, the endpoint, MCP `lint_migration` and the migration advisor score with one
path. So `ALTER COLUMN v TYPE text` over `varchar(20)` reads "no rewrite" in all of
them, `migration.ddl_row_threshold` applies everywhere, and a missing `lock_timeout` is an
annotation everywhere, never a score of its own.
