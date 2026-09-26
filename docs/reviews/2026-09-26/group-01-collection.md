# Group 01 — Collection & observability data plane

Reviewer: G1 agent (read-only). Base: `b396595` (v1.5.0). Paths relative to `sidecar/`.

## Scope (packages/files reviewed, LOC)

| Package / file | Non-test LOC | Notes |
|---|---:|---|
| `internal/collector` | 1,533 | full read |
| `internal/querystore` | 153 | full read + consumers (`executor/rollback.go`, `verify/postgres.go`) |
| `internal/providerobs` | 713 | full read |
| `internal/autoexplain` | 663 | full read + consumers (`optimizer/plancapture.go`, `analyzer/rules_plan_diff.go`) |
| `internal/explain` | 674 | full read + `api/handlers_v09.go` handler |
| `internal/logwatch` | 1,699 | full read + `rca` consumption path |
| `internal/selfmonitor` | 123 | full read |
| `internal/retention` | 183 | full read + FK graph in `internal/schema` |
| `internal/ha` | 117 | full read |
| `cmd/pg_sage_sidecar/{main,metadb,agentdb_fleet,provider_observability}.go` | wiring sections | standalone / YAML-fleet / meta-db / agent-db |
| `internal/schema/*` | DDL of every `sage.*` table this group writes | FK / retention graph |
| **Total in-group** | **5,858** | |

Consumers traced for "written but never read": `analyzer`, `forecaster`, `verify`,
`executor/rollback`, `optimizer`, `rca`, `api/handlers.go` snapshot endpoints.

---

## A. Bugs

| ID | Sev | Conf | file:line | Summary |
|---|---|---|---|---|
| G1-B01 | P0 | CONFIRMED | internal/explain/explain.go:222-226 | Duplicate of G6-B01 (lead-verified; see group-06 file) — fix owned there |
| G1-B02 | P1 | CONFIRMED | `internal/logwatch/parser.go:11-25,87-97` | jsonlog parser expects keys/timestamp PostgreSQL never emits → every real jsonlog line dropped |
| G1-B03 | P1 | CONFIRMED (executed) | `internal/logwatch/tailer.go:174-235`, `watcher.go:149-160` | csvlog records containing newlines (every deadlock DETAIL, multi-line SQL) split per `\n` → CSV parse fails → dropped |
| G1-B04 | P1 | CONFIRMED | `cmd/pg_sage_sidecar/main.go:1828-1885`, `metadb.go:403-491` | Retention runs only in standalone; YAML-fleet and meta-db never purge `sage.*` time-series in monitored DBs (agrees Codex C08) |
| G1-B05 | P1 | CONFIRMED | `internal/collector/queries.go:14-78`, `querystore/querystore.go:98-130`, `verify/postgres.go:53-66` | pg_stat_statements rows not aggregated per `queryid` → duplicate `query_store` samples with identical `captured_at` → nondeterministic windowed latency feeds F1 rollback / verify |
| G1-B06 | P1 | CONFIRMED | `internal/analyzer/analyzer.go:627` vs `collector/snapshot.go:29` | Regression baseline reads `mean_exec_time_ms`; collector persists `mean_exec_time` → all averages 0 → `query_regression` never fires |
| G1-B07 | P1 | CONFIRMED | `cmd/pg_sage_sidecar/main.go:465-466`, `collector/collector.go:219-230` | `HasWALColumns`/`HasPlanTimeColumns` set only in standalone → fleet/meta-db never collect WAL or plan time → RCA WAL-spike tree and high-plan-time rule dead |
| G1-B08 | P1 | CONFIRMED | `internal/collector/queries.go:145-150`, `analyzer/rules_system.go:56-65` | `cache_hit_ratio` emitted as percent (0-100), compared to fraction (0.95) → rule never fires; empty stats → 0 → spurious critical (agrees Codex C01) |
| G1-B09 | P1 | CONFIRMED (code) / PG semantics high-confidence | `internal/autoexplain/collector.go:185-199`, `optimizer/plancapture.go:62-67` | On-demand plans for parameterized queries bind all params to NULL → degenerate plans stored as `auto_explain` and preferred over `GENERIC_PLAN` |
| G1-B10 | P1 | CONFIRMED | `internal/providerobs/runtime_logs.go:12-47`, `supabase_logs.go:44,64-66` | Supabase log cursor never advances on error; window grows each minute → once saturated (≥1000 rows across *all* DBs) or >24 h, ingestion is wedged forever |
| G1-B11 | P2 | CONFIRMED | `internal/logwatch/fanout.go:60-72`, `cmd/.../main.go:1504` | Fleet log fanout copies every cluster signal to every DB's RCA engine (no database filter) |
| G1-B12 | P2 | CONFIRMED | `internal/retention/cleanup.go:31-44`, `schema/bootstrap.go:450,519,605`, `schema/ddl_agent_verify.go:8`, `ddl_agent_ledger.go:21,67`, `ddl_agent_value.go:55` | `action_log`/`findings` purges hit non-cascading FKs → whole batch errors every cycle → never pruned |
| G1-B13 | P2 | CONFIRMED | `cmd/.../main.go:733-735`, `logwatch/tailer_pump.go:98,280-290` | Standalone RCA drains the watcher once per analyzer cycle (600 s); tailer queue caps at 10,000 lines → overflow drops lines |
| G1-B14 | P2 | CONFIRMED | `internal/config/config.go:359,835-841`, `logwatch/classifier.go:195-198` | `exclude_applications` documented "Default: pg_sage" but empty → sidecar's own timeouts become RCA incidents |
| G1-B15 | P2 | CONFIRMED | `internal/autoexplain/collector.go:150-157`, `configure.go:39-59` | Session-level `SET auto_explain.*` on pooled conns, never reset → leaks instrumentation/log noise into all sidecar work |
| G1-B16 | P2 | CONFIRMED | `internal/collector/queries.go:20,35,51,68`, `analyzer/rules_vacuum.go:16-36` | Per-query block I/O time hard-coded 0 → `ioSaturated()` always false |
| G1-B17 | P2 | CONFIRMED | `internal/collector/queries.go:173-184,135-157` | Locks/activity collected cluster-wide; `pg_class` joined by OID across databases → wrong relation names, foreign-DB sessions in snapshot |
| G1-B18 | P2 | CONFIRMED | `internal/collector/collector.go:279-329` | Table keyset cursor persisted on struct survives a mid-pagination error → next snapshot silently partial |
| G1-B19 | P2 | PLAUSIBLE | `internal/collector/collector.go:145-171`, `catalog_query.go:72` | All-or-nothing snapshot across 7 mandatory categories under 500 ms `statement_timeout` → large catalogs yield zero snapshots |
| G1-B20 | P2 | CONFIRMED (code) | `internal/collector/snapshot.go:157-168`, `collector.go:488-498` | Nullable `pg_stat_replication` LSNs scanned into `string` → replicas **and** slots dropped while any walsender has NULL LSN |
| G1-B21 | P2 | CONFIRMED | `internal/ha/ha.go:46-51,64-100` | Safe mode needs 5 consecutive role changes at ~10-min sampling → effectively unreachable; unknown role defaults to primary |
| G1-B22 | P2 | CONFIRMED | `internal/api/handlers_v09.go:233-237` | Explain for an unknown database name silently runs on the primary pool |
| G1-B23 | P2 | CONFIRMED (volume is an estimate) | `internal/collector/collector_helpers.go:68-110` | 11 full JSON categories inserted every 60 s into the *monitored* DB; ~70 % never read; `locks` unbounded |
| G1-B24 | P2 | CONFIRMED (Go semantics) | `internal/logwatch/parser.go:28-35` | Zone abbreviations unknown to the sidecar's TZ parse as UTC → hours of timestamp skew |
| G1-B25 | P2 | PLAUSIBLE | `internal/logwatch/tailer.go:15-17,110-145` | Startup replays last 1 MB of log; dedup is in-memory → restarts re-fire old incidents |
| G1-B26 | P2 | CONFIRMED | `internal/collector/queries.go:26-27`, `executor/rollback.go:316-330` | query_store samples only top-N; target queryid outside top-N → "no data" treated as "no regression" (fail-open verify) |
| G1-B27 | P2 | CONFIRMED | `internal/providerobs/telemetry.go:32-42,47-67` | `Load()` requires Data/Log IO pct that `DeriveTelemetry` never sets → provider `CurrentLoad` always errors |
| G1-B28 | P2 | PLAUSIBLE | `internal/providerobs/supabase.go:42`, `supabase_logs.go:50` | Supabase endpoint paths (`…/endpoints/metrics`, `…/endpoints/logs`) unverifiable offline; live test never evidenced |
| G1-B29 | P3 | PLAUSIBLE | `internal/autoexplain/collector.go:191-194`, `explain/explain.go:218-230` | Failed `EXPLAIN EXECUTE` → `DEALLOCATE` fails in aborted tx; prepared stmt survives ROLLBACK → name collision on that pooled conn |
| G1-B30 | P3 | CONFIRMED | `internal/autoexplain/collector.go:52-56`, `config/config.go:632-665` | `auto_explain.collect_interval_seconds <= 0` unvalidated → `time.NewTicker` panic → process crash |
| G1-B31 | P3 | CONFIRMED | `internal/config/config.go:186`, `collector/circuit_breaker.go:136-167`, `queries.go:258-263` | `cpu_ceiling_pct` documented as host CPU; actually active backends / max_connections |
| G1-B32 | P3 | CONFIRMED | `cmd/.../main.go:2824-2831`, `retention/cleanup.go:73`, `ha/ha.go:49` | Components passing a component name as level (`retention`, `ha`) log errors at INFO |
| G1-B33 | P3 | CONFIRMED | `internal/optimizer/coldstart.go:18` | Cold-start gate counts category rows (11/cycle) via full `count(*)` each optimizer run |
| G1-B34 | P3 | CONFIRMED | `internal/selfmonitor/selfmonitor.go:22-25` | Any query `FROM pg_stat_activity/pg_stat_statements/…` (third-party monitors) treated as pg_sage self-traffic and hidden |
| G1-B35 | P3 | CONFIRMED | `internal/collector/queries.go:186-195` | Sequence `pct_used` ignores `min_value`/descending sequences; `sage` sequences not excluded (agrees Codex) |
| G1-B36 | P3 | PLAUSIBLE | `cmd/pg_sage_sidecar/agentdb_fleet.go:133-150` | Agent-DB collectors persist into DBs never bootstrapped with `sage` schema → ERROR/WARN every 60 s per agent DB |
| G1-B37 | P3 | CONFIRMED | `internal/logwatch/classifier.go:139-145,190-195,372-383` | `maxLinesPerCycle` silently skips later (possibly critical) lines; dedup counts/pids collected but never emitted |
