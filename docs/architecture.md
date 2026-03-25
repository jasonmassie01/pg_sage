# pg_sage Architecture

## Sidecar-Only Decision

pg_sage v0.7+ is a **sidecar-first** product. The Go sidecar is the primary
install path; the C extension is frozen at v0.6.0-rc3.

### Why

1. **Installation friction**: C extensions require `shared_preload_libraries`,
   a PostgreSQL restart, and matching PG major-version binaries. Managed
   services (RDS, Cloud SQL, AlloyDB, Azure) restrict or prohibit custom
   extensions entirely.

2. **Maintenance cost**: The C extension duplicates collector and analyzer
   logic already implemented in Go. Keeping two implementations in sync
   across PG versions (14–17) is not sustainable for a small team.

3. **Feature velocity**: Go ships faster — the optimizer, MCP server, briefing
   worker, and Prometheus exporter all live in the sidecar with no
   extension dependency.

### What the extension still provides

When co-deployed, the extension adds:

- `sage.explain_cache` — automatic EXPLAIN plan capture via executor hooks
- `sage.health()` / `sage.status()` — in-process health checks
- GUC-driven collector/analyzer intervals (no YAML required)

The sidecar detects the extension at startup (`extensionAvailable` flag) and
uses it opportunistically. All core functionality works without it.

## Component Diagram

```
┌──────────────┐     ┌───────────────┐     ┌──────────────┐
│  Collector    │────>│   Analyzer    │────>│   Executor   │
│ (pg_stat_*)   │     │ (rules + LLM) │     │ (DDL actions)│
└──────────────┘     └───────┬───────┘     └──────────────┘
                             │
                    ┌────────┴────────┐
                    │  Optimizer v2   │
                    │ (plan-aware,    │
                    │  HypoPG, dual   │
                    │  model, scored) │
                    └─────────────────┘
```

## Optimizer v2

The index optimizer lives in `internal/optimizer/` and replaces the v1
`internal/llm/index_optimizer.go`. Key improvements:

- **Plan-aware**: Captures EXPLAIN plans via `sage.explain_cache` (extension)
  or `GENERIC_PLAN` (PG 16+) to inform recommendations.
- **HypoPG validation**: Creates hypothetical indexes and measures cost
  improvement before recommending.
- **Dual-model LLM**: Separate LLM client for optimizer (reasoning-tier)
  with independent circuit breaker and token budget.
- **Confidence scoring**: 0.0–1.0 score based on plan data, HypoPG
  validation, query volume, write rate, table size, and index type.
  Maps to action levels: autonomous / advisory / informational.
- **P0 bullet-proofing**: CONCURRENTLY check, column existence, duplicate
  detection, write impact analysis, max indexes per table.
- **Cold start protection**: Waits for N snapshots before running.
- **Post-check**: Verifies `indisvalid` after CREATE INDEX CONCURRENTLY.

## Data Flow

1. **Collector** gathers `pg_stat_statements`, `pg_stat_user_tables`,
   `pg_stat_user_indexes` every 60s.
2. **Analyzer** runs rules every 600s, then calls `optimizer.Analyze()`.
3. **Optimizer** enriches table contexts with `information_schema.columns`,
   `pg_stats`, plan data, and workload classification.
4. LLM generates index recommendations as JSON.
5. **Validator** runs P0 checks; **HypoPG** validates if available.
6. **Confidence scorer** assigns action level.
7. Findings are persisted to `sage.findings`.
8. **Executor** acts on findings based on trust level and confidence.
