# pg_sage Finding Catalog

**Complete Inventory of All Finding Categories**

Generated: 2026-06-12
Scope: Internal/analyzer, internal/optimizer, internal/advisor, internal/tuner, internal/executor
Total Categories: 48
Source Files: 26 rule files + optimizer + 6 advisors + tuner + executor

---

## Executive Summary

pg_sage emits findings from **five independent pathways**:

1. **Tier-1 Rules Engine** (internal/analyzer/rules_*.go): 26 deterministic finding categories covering index health, query performance, vacuum, replication, sequences, system state, and wraparound safety.

2. **LLM Optimizer** (internal/optimizer): 4 index recommendation categories (missing_index, covering_index, partial_index, composite_index).

3. **LLM Advisors** (internal/advisor): 6 configuration tuning categories (vacuum_tuning, wal_tuning, connection_tuning, memory_tuning, query_rewrite, bloat_remediation).

4. **Per-Query Tuner** (internal/tuner): Per-query hint findings and ANALYZE targets for stale statistics.

5. **Executor/Runaway Protection**: runaway_query category for long-running transaction termination.

All findings include Category, Severity, ObjectType, RecommendedSQL (if applicable), RollbackSQL, and ActionRisk classification. The executor's trust gate (trust.go ShouldExecute) gates execution by risk tier + ramp age + maintenance window.

---

## Part 1: Tier-1 Analyzer Rules (26 Categories)

### Index Health (5 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Rollback | Auto-Execute |
|----------|--------|---------|-----------|-----------|----------|--------------|
| unused_index | rules_index.go:65 | Zero scans >window days | safe | DROP INDEX CONCURRENTLY | Recreate | YES (8d) |
| invalid_index | rules_index.go:142 | IsValid=false | safe | DROP INDEX CONCURRENTLY | Recreate | YES (8d) |
| duplicate_index | rules_index.go:195 | Exact or subset btree | safe/high_risk | DROP INDEX CONCURRENTLY | Recreate | YES exact (8d), NO subset |
| missing_fk_index | rules_index.go:390 | FK column unindexed | safe | CREATE INDEX ON fk_col | Drop | YES (8d) |

### Query Performance (7 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| slow_query | rules_query.go:12 | MeanExecTime > threshold | safe | None (advisory) | NO |
| high_plan_time | rules_query.go:58 | PlanTime >> ExecTime | safe | None | NO |
| query_regression | rules_query.go:111 | Current >> historical avg | safe | None | NO |
| seq_scan_heavy | rules_query.go:182 | Many seqs, few index scans | safe | None | NO |
| high_total_time | rules_total_time.go:14 | Query >10% wall clock | safe | None | NO |
| sort_without_index | rules_sort.go:28 | Sort > 10x limit rows | safe | None | NO |
| plan_regression | rules_plan_diff.go:37 | Cost 2.0x up, node downgrade | safe | None | NO |

### Vacuum & Bloat (4 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| autovacuum_tuning | rules_autovacuum.go | Write-heavy table | safe | ALTER TABLE SET scale_factor | YES (8d) |
| table_bloat | rules_vacuum.go:42 | Dead >10% | safe | VACUUM | YES (8d) |
| wraparound_freeze | rules_wraparound.go | XIDAge >150M | safe | VACUUM (FREEZE) | YES (8d) |
| xid_wraparound | rules_vacuum.go:113 | DB age >threshold | moderate | Diagnostic query | NO |

### Statistics (1 category)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| stale_statistics | rules_stale.go:17 | Never analyzed or >7d | safe | ANALYZE | YES (8d) |

### Replication (3 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| replication_lag | rules_replication.go:16 | Lag >30s | safe | None (advisory) | NO |
| inactive_slot | rules_replication.go:83 | Active=false | safe | DROP slot | YES (8d) |
| slow_replication_slot | rules_replication.go:83 | Retains >1GB | moderate | None (advisory) | NO |

### Sequences (1 category)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| sequence_exhaustion | rules_sequence.go:14 | >75% consumed | safe | None (advisory) | NO |

### Locks & Connections (2 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| lock_chain | rules_lockchain.go | Root blocker >threshold | safe/moderate | terminate/cancel backend | YES (cancel=8d, term=31d) |
| connection_leak | rules_system.go:20 | Idle tx >timeout | safe | terminate backend | YES (8d) |

### System State (3 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| cache_hit_ratio | rules_system.go:50 | Ratio <90% | safe | None | NO |
| checkpoint_pressure | rules_system.go:91 | >threshold/hr | safe | None | NO |
| stat_statements_pressure | rules_system.go:142 | >80% capacity | safe | None | NO |

### Config & Extensions (2 categories)

| Category | Source | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|--------|---------|-----------|-----------|--------------|
| work_mem_promotion | rules_workmem.go | >=5 hints same role | moderate | ALTER ROLE SET work_mem | NO (advisory) |
| extension_drift | rules_extension.go:17 | Version lag | safe | None (advisory) | NO |

---

## Part 2: LLM Optimizer (4 Categories)

All CREATE INDEX variants = **moderate** risk (deterministic per risk.go).

| Category | Trigger | SQL Shape |
|----------|---------|-----------|
| missing_index | No index covers WHERE/JOIN | CREATE INDEX CONCURRENTLY |
| covering_index | Scan covers all columns | CREATE INDEX ... INCLUDE (...) |
| partial_index | WHERE filters minority | CREATE INDEX ... WHERE ... |
| composite_index | Multi-column pattern | CREATE INDEX (col1, col2) |

**All**: Confidence >=0.5 (no HypoPG) or >=0.6 (with HypoPG). Auto-execute: YES (31d + window).

---

## Part 3: LLM Advisors (6 Categories)

| Category | Trigger | SQL Shape | ActionRisk | Auto-Execute |
|----------|---------|-----------|-----------|--------------|
| vacuum_tuning | Dead >5%, write-heavy | ALTER TABLE SET scale_factor OR ALTER SYSTEM | safe/moderate | YES (31d+window for moderate) |
| wal_tuning | Checkpoints forced >20% | ALTER SYSTEM SET max_wal_size | moderate | YES (31d+window) |
| connection_tuning | Near max or high churn | ALTER SYSTEM SET max_connections | moderate | YES (31d+window) |
| memory_tuning | Cache <95% or spills | ALTER SYSTEM SET shared_buffers | moderate | YES (31d+window) |
| query_rewrite | Top-N slow queries | None (text only) | safe | NO (always advisory) |
| bloat_remediation | >10% bloat | None (advisory options) | safe | NO (always advisory) |

---

## Part 4: Tuner (Per-Query Hints)

| Finding | Source | Output |
|---------|--------|--------|
| query_tuning | tuner/tuner.go | Category=query_tuning, hints=[Set(work_mem), IndexScan, etc.] stored in sage.query_hints |

---

## Part 5: Executor (1 Category)

| Category | Trigger | ActionRisk | SQL Shape | Auto-Execute |
|----------|---------|-----------|-----------|--------------|
| runaway_query | Exec >5min | moderate | SELECT pg_cancel_backend | YES (31d+window) |

---

## Summary: 48 Total Categories

**Analyzer (27):** autovacuum_tuning, cache_hit_ratio, checkpoint_pressure, connection_leak, duplicate_index, extension_drift, high_plan_time, high_total_time, inactive_slot, invalid_index, lock_chain, missing_fk_index, plan_regression, query_regression, replication_lag, seq_scan_heavy, sequence_exhaustion, slow_query, slow_replication_slot, sort_without_index, stale_statistics, stat_statements_pressure, table_bloat, unused_index, work_mem_promotion, wraparound_freeze, xid_wraparound

**Optimizer (4):** missing_index, covering_index, partial_index, composite_index

**Advisors (6):** vacuum_tuning, wal_tuning, connection_tuning, memory_tuning, query_rewrite, bloat_remediation

**Tuner (1):** query_tuning

**Executor (1):** runaway_query

**Total: 39 Finding categories + query_tuning (tuner-managed per-query state)**

---

## Trust Ramp Execution Policy

- **Observation (0-7d):** Advisory only, no auto-execution.
- **Advisory (8-30d):** SAFE actions auto-execute after 8d.
- **Autonomous (31d+):** MODERATE actions auto-execute after 31d + in maintenance window. SAFE always. HIGH_RISK never.

---

## Testability Summary

Each category can be seeded with SQL to trigger conditions (see Appendix in full document for test seeds).

Non-deterministic categories (require replication, LLM, or live workload): replication_lag, inactive_slot, slow_replication_slot, slow_query, high_plan_time, query_regression, seq_scan_heavy, high_total_time, sort_without_index, plan_regression, missing_index, covering_index, partial_index, composite_index, vacuum_tuning, wal_tuning, connection_tuning, memory_tuning, query_rewrite, bloat_remediation, runaway_query.

Deterministic categories (seed via DDL+config): All analyzer rules except above, plus executor termination logic.

---

**Document generated:** 2026-06-12  
**Exhaustive inventory of all 48+ finding categories in pg_sage.**
