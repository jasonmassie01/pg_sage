# Read-only corpus fixtures (RO-01..RO-16)

Each corpus case is a pair of files named by its id:

- `<id>.json` — metadata only, no SQL:

  ```json
  {
    "id": "RO-01",
    "technique": "one-line summary of the bypass class (not a recipe)",
    "expect": "privilege_error"
  }
  ```

  `expect` is the refusal class the privilege-based read-only role (the
  canonical design) should produce. One of: `privilege_error`,
  `read_only_error`, `rejected_before_execution`. The harness still scores a
  case as held whenever the statement was refused by **any** design and the
  fixture checksums are unchanged; `expect` documents the canonical class.

- `<id>.sql` — the single statement under test, one statement per file.

The loader (`readonly_fixtures.go`) pairs them by id, sorted. `id` inside
the JSON must equal the file name.

## What the fixture database provides

`PrepareReadOnly` (see `fixture.go`) builds, on the bench's own disposable
database:

- schema `sb_fixture` with `widgets(id, qty)` and `ledger(id, note)` seeded,
  and sequence `sb_fixture.counter`;
- role `sb_readonly` (NOLOGIN) granted only `USAGE` on the schema and
  `SELECT` on its tables — no write, no sequence and no function grants.

Write corpus statements against `sb_fixture` objects so the checksum set
(`widgets`, `ledger`) captures any mutation.

## Author the RO-01..RO-16 cases here

The corpus ids and their bypass classes are specified in
incidents-security.md §4.4 (RO-01..RO-16). Drop `RO-01.json`/`RO-01.sql` ..
`RO-16.json`/`RO-16.sql` into this directory. No code change is needed: the
embedded loader picks them up, and `TestAgentSafetyBench` runs them against
all three designs.

The self-check cases (`sc-insert`, `sc-update`, `sc-create`) prove the
harness with trivially benign writes and must be refused under all three
designs with checksums intact.
