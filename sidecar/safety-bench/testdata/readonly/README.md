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
  `read_only_error`, `rejected_before_execution`. Each design is scored on
  its own: held when it refused the statement with any credited class and
  the fixture checksums are unchanged. `expect` is compared with the class
  `privilege_role` observed; a different credited class fails the bench (the
  case no longer exercises what it declares), while an execution or an
  `other_error` is listed and scored as not held or not measured.

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

## The RO-01..RO-16 cases

The corpus ids and their bypass classes are specified in
incidents-security.md §4.4 (RO-01..RO-16); each case here is one
representative statement of its class, written against `sb_fixture` only.
`expect` is `privilege_error` throughout: the privilege-based role is the
canonical design. RO-15 (lifting the statement timeout) is expected to
execute under the transaction and role designs: neither bounds resources,
and the report says so rather than hiding it. Add a case by dropping a new
`<id>.json`/`<id>.sql` pair here; the embedded loader picks it up.

The self-check cases (`sc-insert`, `sc-update`, `sc-create`) prove the
harness with trivially benign writes and must be held under all four
designs: a credited refusal (never `other_error`) with checksums intact.
