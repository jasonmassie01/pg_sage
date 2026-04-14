# Gemini 2.5 Pro Review: v0.10 Schema Intelligence Spec

**Date**: 2026-04-13
**Model**: gemini-2.5-pro via Gemini CLI 0.36.0
**Reviewer**: External LLM review

---

## Overall Assessment

- **Positive**: Thesis is strong. Database-level N+1 detection is a killer feature. Runtime-aware DDL safety is a massive improvement over static CI checks. Feature set is cohesive and targets high-value problems.
- **Concerns**: Timeline is highly ambitious. Spec underestimates difficulty of correctly parsing SQL and handling DDL on partitioned tables. Some detection methods have subtle but important edge cases.

---

## Feedback by Section

### 1. DDL Safety Advisor

**1.3 DDL Detection — PL/pgSQL blind spot**
- `log_statement = 'ddl'` logs the `CALL` statement, not the `ALTER TABLE` within a function/procedure. DDL inside PL/pgSQL only caught by activity_polling fallback if long-running.
- **Amendment**: Add limitations subsection acknowledging this.

**1.4 DDL Classification — SQL parser recommendation**
- Custom tokenizer is fragile. Quoted identifiers, complex comments, unusual whitespace will break it.
- Example: `ALTER TABLE "my_table" ADD COLUMN "description" TEXT DEFAULT '-- Do not use';`
- **Amendment**: Consider `pg_query_go` for robust AST-based parsing instead of custom tokenizer.

**1.4 Missing Rule — ADD COLUMN NOT NULL (pre-PG11)**
- `ALTER TABLE ... ADD COLUMN ... NOT NULL` required full table rewrite + ACCESS EXCLUSIVE pre-PG11.
- **Amendment**: Add rule `ddl_add_column_not_null` with PG11+ version gate.

**1.5 Risk Assessment — Replication lag detection**
- Calculating `ReplicationLag` non-trivial. Need `pg_is_in_recovery()` first, then different queries for primary vs replica.
- **Amendment**: Specify lag detection logic: `pg_stat_replication` on primary, LSN comparison on replica. Note required permissions.

### 2. Schema Anti-Pattern Detection

**2.3.2 lint_unused_index — stats reset sensitivity**
- Index might be used for quarterly reports, appearing "unused" for months.
- **Amendment**: Include `pg_stat_database.stats_reset` timestamp in findings. Caveat recommendations to monitor over full business cycle.

**Missing Rule — Low cardinality indexes**
- Indexes on boolean/enum columns often ignored by planner, cause write overhead.
- **Amendment**: Add `lint_low_cardinality_index` using `pg_stats.n_distinct`.

### 3. N+1 Query Detection

**3.2 Detection — Regex too simplistic**
- `query ~* 'SELECT.*FROM.*WHERE.*=\s*\$'` misses multi-WHERE patterns like `WHERE id = $1 AND tenant_id = $2`.
- **Amendment**: Use SQL parser for AST-based WHERE clause analysis instead of regex.

**3.2 Parent-child correlation — Feasibility concern**
- Most complex and fragile part of spec. Heuristics may produce false positives in busy systems.
- 2-3 week estimate is optimistic.
- **Amendment**: Start with high confidence threshold. Provide clear UI for dismissing incorrect findings.

### 6. Operational Constraints

**6.3 Partitioned Tables — VALIDATE CONSTRAINT**
- Cannot validate constraint with single `VALIDATE CONSTRAINT` on parent. Must iterate over child partitions individually.
- **Amendment**: Safe alternative generation must be partition-aware, generating multi-statement DDL.

---

## Suggested Additions (Future Versions)

1. **Autovacuum Intelligence**: Analyze if autovacuum keeping up, blocked by locks, per-table tuning
2. **Index Advisor**: Recommend new indexes from high-cost pg_stat_statements queries
3. **Configuration Linter**: Check postgresql.conf against best practices

## Timeline Recommendation

Extend from 6 weeks to 8-10 weeks for robust implementation and testing of complex features (SQL parsing, partitioned table DDL).
