# Gemini 3.1 Pro Preview Review: Shipped RCA Specs (v0.9 + v0.9.1)

**Date**: 2026-04-14
**Model**: gemini-3.1-pro-preview via Gemini CLI 0.36.0
**Specs reviewed**: v0.9-root-cause-prevention.md, v0.9.1-log-based-rca.md

---

## Overall Assessment

Architecture is "structurally excellent and highly competitive." Tiered deterministic/LLM approach, autonomous remediation, and Self-Action correlation are standout features. However: fundamental PG internals errors in deterministic trees, fatal EXPLAIN parameterization flaw, and architectural race conditions in high-volume logging.

---

## 1. Technical Correctness Issues

### 1.1 EXPLAIN Parameterization Flaw (v0.9 §7.4) — CRITICAL
- Cannot run `EXPLAIN` on pg_stat_statements normalized queries (`WHERE id = $1`) — PG throws `ERROR: there is no parameter $1`
- Must either: intercept unnormalized query from pg_stat_activity/logs, replace params with NULL via PREPARE, or require user to pass Params via API
- **Status**: Needs investigation against shipped code

### 1.2 OOM vs Temp File Logic REVERSED (v0.9.1 §5.2) — CRITICAL
- Spec says temp files indicate "memory exhaustion" and recommends increasing work_mem
- REALITY: Temp files PREVENT OOM. PG spills to disk when respecting work_mem
- If actual OOM occurs, should recommend DECREASING work_mem or lowering max_connections
- **Fix**: Reverse the decision tree logic

### 1.3 Lock Relation Ambiguity (v0.9 §4.3) — P1
- Migration/transaction may hold locks on hundreds of relations
- Query returns all of them indiscriminately
- **Fix**: Aggregate with `string_agg(DISTINCT ..., ', ') LIMIT 5`, cross-reference with what blocked processes are actually waiting on

---

## 2. Missing Edge Cases & Signal Gaps

### 2.1 Orphaned Prepared Transactions — CRITICAL GAP
- `PREPARE TRANSACTION` (2PC) survives connection drops and server restarts
- Holds xmin and locks indefinitely
- NEVER shows in pg_stat_activity — invisible to idle-in-transaction detection
- **Fix**: Add `pg_prepared_xacts` to Snapshot. New Tier 1 branch for vacuum_blocked and lock_contention trees.

### 2.2 XID Wraparound Misdiagnosis (v0.9.1 §5.2) — P0
- Recommending `VACUUM FREEZE` is wrong — autovacuum is already trying and being blocked
- Root cause is what holds the xmin horizon back: stale backend_xmin, replication slots (catalog_xmin), or prepared transactions
- **Fix**: Cross-reference with pg_stat_activity oldest xmin, replication slot catalog_xmin, pg_prepared_xacts

### 2.3 Active Replication Slots (v0.9 §3.2) — P0
- vacuum_blocked only checks `Active=false` slots
- Active slots with slow consumers (Debezium, Fivetran, struggling replica) are equally dangerous
- **Fix**: Remove `AND Active=false` from threshold check. Active slow slots = "slow consumer holding WAL/xmin"

---

## 3. Architecture Issues

### 3.1 Log Buffer FIFO Drop Risk (v0.9.1 §2.3) — P0
- 10,000 signal buffer with FIFO overflow
- Broken app can spam 10K duplicate key errors in 2 seconds, pushing out FATAL/PANIC signals
- **Fix**: Priority-based ring buffer. Never drop CRITICAL/FATAL/PANIC. Drop LOG/INFO first.

### 3.2 Deduplication Masks Outage Duration (v0.9 §3.5) — P1
- DetectedAt updates to now() on dedup, masking how long issue has persisted
- 12-hour outage looks like 29-minute issue
- **Fix**: Keep FirstDetectedAt (static) + LastDetectedAt (sliding). Expose both in API.

### 3.3 Linear Forecast False Positives (v0.9 §5.6) — P2
- DB growth is step-function (bulk ingest), not linear
- OLS regression triggers false positives during steps, false negatives during plateaus
- **Fix**: Consider Holt-Winters or 90th percentile of daily deltas over 30-day window

---

## 4. Competitive Assessment

### Moats (where pg_sage leads)
1. **Self-Action Correlation** — "brilliant product decision." No competitor audits its own blast radius.
2. **Deterministic + LLM Hybrid** — correct architecture for MTTR reduction
3. **Active Lock Chain Resolution** — trust-gated termination vs passive dashboards

### Gaps (where pg_sage trails)
1. **Wait Event Profiling** — no deep wait_event analysis (DataFileRead vs LWLock vs ClientRead). pganalyze excels here.
2. **Connection Pooling Awareness** — no PgBouncer/Odyssey visibility. 50% of connection storms originate in external poolers.
3. **Query Parameter Inference** — needed for EXPLAIN on ORM workloads. pganalyze has an advanced parser for this.

---

## Summary: Priority Fixes

| # | Finding | Priority | Effort |
|---|---------|----------|--------|
| 1.1 | EXPLAIN parameterization flaw | P0 | Investigation needed |
| 1.2 | OOM/temp file logic reversed | P0 | Decision tree fix |
| 2.1 | Orphaned prepared transactions | P0 | New signal + tree branch |
| 2.2 | XID wraparound misdiagnosis | P0 | Cross-reference fix |
| 2.3 | Active replication slot check | P0 | Remove Active=false filter |
| 3.1 | Log buffer priority drops | P0 | Priority ring buffer |
| 1.3 | Lock relation aggregation | P1 | Query fix |
| 3.2 | Dedup masks duration | P1 | Add FirstDetectedAt |
| 3.3 | Linear forecast false positives | P2 | Future ML upgrade |
