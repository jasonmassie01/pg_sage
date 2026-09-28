# Azure flavor matrix, 2026-09-27

Live run of `scripts/azure/flavor-matrix.sh` in centralus. Each flavor was provisioned, run
through the checklist in docs/azure.md, and then deleted. No resources remain; the empty
resource group `pg-sage-test` is kept for reuse.

| Flavor | Server | AZ-01 detect | AZ-02 setup SQL | AZ-03 collect | AZ-06 ARM wired | AZ-07 dynamic param | AZ-08 restart-bound | Result |
|---|---|---|---|---|---|---|---|---|
| flex-11 | Burstable B1ms | PASS | PASS | refused (PG14+) | refused | PASS | PASS | unsupported by design |
| flex-12 | Burstable B1ms | PASS | PASS | refused (PG14+) | refused | PASS | PASS | unsupported by design |
| flex-13 | Burstable B1ms | PASS | PASS | refused (PG14+) | refused | PASS | PASS | unsupported by design |
| flex-14 | Burstable B1ms | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| flex-15 | Burstable B1ms | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| flex-16 | Burstable B1ms | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| flex-17 | Burstable B1ms | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| flex-18 | Burstable B1ms | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| flex-gp (PG18) | General Purpose D2ds_v5 | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| elastic (PG18) | Elastic cluster, 2 × D2ds_v5 | PASS | PASS | PASS (22) | PASS | PASS | PASS | **PASS** |
| cosmos | Cosmos DB for PostgreSQL | not creatable | | | | | | Azure refuses new clusters (service retirement) |

Not creatable at all: Single Server (retired 2025) and HorizonDB (gated preview).

CHECK-AZ-04 (fleet readiness in the dashboard) and CHECK-AZ-05 (an approved ANALYZE) are
MANUAL and were not run.

## Findings

1. **Bug, fixed in 7c425db.** Azure reports memory units in PostgreSQL's spelling (`kB`,
   `8kB`). The converter matched only `KB`, so every memory-valued server-parameter change
   was refused. The smoke run on PG17 found it. After the fix, AZ-07 passes on every
   flavor.
2. **Found while building the matrix, fixed in 4efca0a.** Cosmos DB for PostgreSQL hosts
   (`*.postgres.cosmos.azure.com`) were not detected as Azure. They are now `azure-cosmos`:
   portable SQL actions run, and server parameters are guidance-only. Existing clusters
   benefit, but this is covered by unit tests only, since no cluster can be created to
   test live.
3. PG 11–13 are still creatable on Azure (extended support). pg_sage refuses them at
   startup with "PostgreSQL 14+ required", as designed. The ARM layer works on them
   regardless.
4. The documented setup SQL, including `pg_signal_backend`, `CREATE SCHEMA ... AUTHORIZATION`
   and HypoPG/pg_hint_plan, runs without error as the Azure admin on every flavor, elastic
   clusters included.
