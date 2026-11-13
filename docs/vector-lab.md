# Vector Evidence Lab

`pg_sage vector-lab` measures filtered HNSW recall and latency on a target you
choose, in one read-only, repeatable-read transaction. It never applies a setting:
every knob is `SET LOCAL` and is rolled back, and `auto_apply` is always false.

```bash
SAGE_VECTORLAB_DATABASE_URL=postgres://reader@clone/db \
  pg_sage vector-lab --manifest workload.json
```

Exit **0**: at least one variant qualified and is named in `recommendation`.
Exit **2**: the experiment completed but no variant qualified; read each
variant's `reasons`. Exit **1**: invalid input or a failed experiment.

Requires pgvector 0.8.0 or later (the version that added iterative scans and
their limits). Older versions are refused with the version found; a manifest with
a `relaxed_order` variant names that mode in the error. On Lakebase this means
its pgvector must be 0.8 or later.

## Target

- An ordinary table with a single-column, non-null, unique, valid identity key.
- `vector_column` must be a pgvector `vector` or `halfvec` column. The type is
  read from the catalog, never from the manifest, and the query vector is cast
  to that type.
- Query dimensions: 1..2000 for `vector`, 1..4000 for `halfvec` (the HNSW
  limits). A 3072-dimension embedding therefore needs a `halfvec` column.
- Distances `l2`, `cosine`, `inner_product`; up to 8 equality filters.
- Row-level security follows the connected role.

## Manifest fields

| Field | Range | Notes |
|---|---|---|
| `k` | 1..100 | rows per query |
| `repeats` | 1..20 | measurements per query |
| `min_recall` | (0, 1] | worst per-trial recall a variant may show |
| `max_p95_ms` | (0, 60000] | client-observed p95 per variant |
| `statement_timeout_ms` | 1..30000 | also the lock timeout |
| `total_timeout_ms` | 1..600000 | whole-run deadline |
| `queries` | 3..200 | each with `id`, `vector`, `filters` |
| `variants` | 1..16 | see below |

Queries x repeats x (variants + 1) must stay at or under 10000 measured queries.

Each variant:

| Field | Range | Default | pgvector setting |
|---|---|---|---|
| `name` | unique, <=80 bytes | required | |
| `ef_search` | 1..1000 | required | `hnsw.ef_search` |
| `iterative_scan` | `off`, `strict_order`, `relaxed_order` | required | `hnsw.iterative_scan` |
| `max_scan_tuples` | 1..1000000 | 20000 | `hnsw.max_scan_tuples` |
| `scan_mem_multiplier` | 1..1000 | 1 | `hnsw.scan_mem_multiplier` |

A selective filter (say 0.5% of rows) can underfill every variant at the default
20000 scanned tuples. Raise `max_scan_tuples` for that variant; the statement
timeout still bounds the work. `scan_mem_multiplier` multiplies the 16 MB
transaction-local `work_mem` the scan may use.

`relaxed_order` may return rows slightly out of distance order. The lab re-sorts
those k rows before scoring; `off` and `strict_order` rows that arrive out of
order still abort the experiment as invalid.

## Recall and the tie rule

The exact baseline runs with index scans off and fetches k+1 rows. When the k-th
and (k+1)-th exact distances are equal, any of the tied rows is a correct k-th
answer. The lab then fetches every exact row at or nearer than the k-th distance
(at most 2k rows). Rows strictly nearer than the k-th distance belong in every
correct answer; the tied rows may fill only the places left after them. So a
candidate row matches when it is a nearer row, or a tied row while tied places
remain: recall = (nearer matches + min(tied matches, k - nearer rows)) / k. A tied
row can stand in for another tied row, never for a missed nearer one. If more than
2k rows tie, the set is cut at 2k, which can only lower recall, never raise it. The boundary
query runs once per tied query and repeat under the same statement timeout and
whole-run deadline; it is not counted in the measured-query limit or exact latency.

A tie is information, not a failure: the query shows
`"ambiguous_boundary_tie": true` and the report counts tied queries in
`ambiguous_boundary_ties`. Corpora with duplicate embeddings can now qualify.

## Report

`format_version` is **2**. Version 2 adds `vector_type`, `ambiguous_boundary_ties`
and, only when the manifest set them, `max_scan_tuples` and `scan_mem_multiplier`
under each variant's `settings`. A tie no longer appears in `reasons`. For a
manifest without the new fields the report is otherwise the same as version 1.

Reasons a variant does not qualify: `empty_ground_truth`, `underfilled`,
`recall_below_target`, `hnsw_not_used`, `latency_above_target`.

Row IDs, vectors, filter values and connection details are never written to the
report. Query labels are, so use opaque ones.
