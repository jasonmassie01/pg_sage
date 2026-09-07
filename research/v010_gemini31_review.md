# Gemini 3.1 Pro Preview Review: v0.10 Schema Intelligence Spec

**Date**: 2026-04-14
**Model**: gemini-3.1-pro-preview via Gemini CLI 0.36.0

---

## Mandatory Fixes (4 items)

1. **Bloat detection is fundamentally flawed** — `n_dead_tup / (n_live_tup + n_dead_tup)` measures pending vacuum work, NOT bloat. A table with 0 dead tuples can be 90% bloated. Use `relpages` vs expected pages heuristic (pgstattuple-style).

2. **Missing multixact wraparound** — `lint_txid_age` only monitors `relfrozenxid`. Must also monitor `relminmxid` for multixact exhaustion (FK + SELECT FOR SHARE/UPDATE heavy workloads cause same catastrophic shutdown).

3. **DDL risk must include lock queue cascade** — Risk assessment looks at active queries but misses the cascade: waiting DDL blocks ALL subsequent queries. If ANY long-running transaction holds a lock, ACCESS EXCLUSIVE risk should spike to critical immediately.

4. **pg_query_go must be hard requirement** — Not optional. Regex/tokenization of PG DDL is a fool's errand. Multi-line strings, nested comments, standard-conforming strings will break immediately.

## Technical Corrections (3 items)

5. **Matview base tables** — Cannot parse SQL strings for base tables. Must use recursive CTE against `pg_depend` (deptype='n') to catch nested views.

6. **Transactional DDL** — DDL is transactional in PG. Lock held until COMMIT. Must check if session is `idle in transaction` after DDL and warn about DDL mixed with long transactions.

7. **UNLOGGED tables** — Don't generate WAL, so replication lag doesn't apply. Risk assessment must check `pg_class.relpersistence = 'u'` and skip repl_factor.

## Missing Edge Cases (2 items)

8. **ATTACH PARTITION without CHECK** — Requires ACCESS EXCLUSIVE on child + SHARE UPDATE EXCLUSIVE on parent. Without matching CHECK constraint, PG table-scans the child under lock. Add `ddl_attach_partition_no_check` rule.

9. **N+1 false negatives** — If same query used by both N+1 loop AND healthy API endpoint, metrics are obfuscated. Document as explicit limitation.

## Competitive Gaps (2 items)

10. **HypoPG integration** — Create hypothetical index + EXPLAIN to prove cost reduction before suggesting index. Would make pg_sage "practically magical."

11. **CI/CD CLI** — `pg_sage assess --file migration.sql` for GitHub Actions/GitLab CI. Shift-left is more valuable than runtime detection.

## Architecture (2 items)

12. **lint.Rule should emit Incidents directly** — Instead of `ToAnalyzerFinding()` coercion, unify domain models.

13. **query_hash should use PG's fingerprinting** — Match PG14's queryid algorithm for consistency, or use normalized text hash.

## Timeline

12 weeks recommended. N+1 and Matview could be separate releases (v0.11, v0.12).
