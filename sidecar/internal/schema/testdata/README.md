# schema testdata

`v2.4.0_sage_dump.sql` is the `sage` schema, with its rows, of an empty
database that pg_sage v2.4.0 bootstrapped twice. `upgrade_db_test.go`
restores it and boots the current code over it (config-04).

How it was made (PostgreSQL 14, so the dump restores on 14 to 18):

1. `git archive v2.4.0 sidecar/go.mod sidecar/go.sum sidecar/internal`,
   extracted to a scratch directory, plus a small `main` that calls
   `schema.Bootstrap` twice on a new database `v240`.
2. `pg_dump -d v240 --schema=sage --inserts --no-owner --no-privileges
   --no-comments > v2.4.0_sage_dump.sql`, then the `\restrict` /
   `\unrestrict` psql lines removed.

`migration_checksums.txt` pins the SHA-256 of every migration step's SQL
as `sage.schema_migrations` records it (config-04b). A released step must
never change: every install's next boot would fail on the checksum. Add a
new step instead, and append its line (the failing test prints it).

`ddl_constants.sha256` pins the SHA-256 of every base-table DDL constant,
every `expectedTables` entry, every migration step and the audit chain's DDL
(config-31). It was generated on the unsplit `bootstrap.go`, before the
constants moved to `ddl_core_tables.go` and `ddl_core_alters.go`, and covers
the base tables a fresh install runs, which the ledger never sees. A pinned
line never changes; a new migration step needs no line here (the ledger pins
it), but a new base table or `expectedTables` entry does (the failing test
prints it).
