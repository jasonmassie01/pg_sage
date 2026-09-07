# Product research and Vector Evidence Lab

- [Product report](report.md): repository intent, real support cases, competitor
  analysis, vector and agent-database opportunities, ranked roadmap.
- [Build specification](build-spec.md): implemented first slice and acceptance contract.
- [Test results](test-results.md): commands, counts, failures, coverage and limits.
- [Example workload](example-workload.json) and [synthetic fixture](demo-fixture.sql).

## Run the implemented feature

Build the normal sidecar binary from `sidecar/`:

```powershell
go build -o pg_sage.exe ./cmd/pg_sage_sidecar
$env:SAGE_VECTORLAB_DATABASE_URL = $env:MY_READ_ONLY_CLONE_URL
./pg_sage.exe vector-lab --manifest ../research/2026-09-04-product/example-workload.json
```

Use a read-only role on the selected clone or target, and edit the manifest to
match its table, distance, vector dimensions, filters and representative query
vectors. For a local synthetic demonstration, the fixture file creates a separate
`pgsage_vectorlab_demo` schema in a disposable database. It requires write access
for fixture setup only; the experiment command has no schema setup or mutation path.

Output is JSON. Exit **0** means at least one supplied HNSW configuration met
every sampled recall/underfill/plan requirement and the p95 budget. Exit **2** means
the experiment completed but no candidate qualified; consult each candidate's
`reasons`. Exit **1** means invalid input or a failed experiment. No configuration
is applied; `auto_apply` is always false.

`exact_p95_ms` is a comparison baseline, while `recommendation` selects only among
the supplied HNSW candidates. A qualifying result does not necessarily outperform
exact search. All timing is client observed on this connection with warm data;
the exact baseline fetches one extra row to detect boundary ties. Three query
samples make a useful smoke test, not a representative production workload.

The report records a manifest hash, snapshot, version metadata, explicit settings,
worst recall per query, underfilled trial count, HNSW plan index names and p95.
Row IDs, vectors, filter values and connection details are omitted. Use opaque
query labels because labels are included. Store workload manifests privately.

Limits are explicit: 3..200 queries, 1..16 candidates, 1..20 repeats, k <=100,
<=10000 measured queries, <=30 seconds per statement, <=10 minutes total,
one connection, transaction-local 16 MB work_mem and bounded iterative scan work.
Exact search can still consume I/O and temporarily retain a snapshot. Each
candidate also has a nonexecuting EXPLAIN; measured-query count excludes EXPLAIN
and transaction-control statements. The total deadline covers all of them.

This release supports ordinary tables with a valid single-column non-null unique
identity, pgvector `vector` columns, L2/cosine/inner-product distance and equality
filters. RLS follows the connected role. Empty truth, ties at the kth boundary,
missing HNSW plans or a failed cohort produce no recommendation. Partition roots,
views, halfvec/expression indexes, DiskANN, arbitrary SQL, hybrid/reranking,
rebuilds, automatic cloud clones and persistent policy changes are outside this
implemented slice. Existing vector versions below 0.8.0 are rejected explicitly.

The next integration is a versioned Cases/ledger attachment with workload/schema
identity and expiry. The existing latency verification engine cannot certify
vector recall and is intentionally not granted new autonomous authority here.
