# Tier 1 — Collector & Deterministic Rules Engine

Reverse-engineered from source. Module `github.com/pg-sage/sidecar`, Go 1.24, PostgreSQL 14+.
File:line citations are to `sidecar/internal/...`. Config defaults are in
`internal/config/defaults.go`; structs/keys in `internal/config/config.go`.

---

## 1. Collector (`internal/collector`)

### 1.1 Lifecycle

`Collector.Run` (`collector.go:48`) ticks on `cfg.Collector.Interval()`
(default **60s**, `defaults.go:15`). Each tick calls `cycle` (`collector.go:66`):

1. **Circuit breaker gate** (`circuit_breaker.go`). `ShouldSkip` runs `loadRatioSQL`
   (`queries.go:248`): `active_backends / max_connections`. If `loadRatio > CPUCeilingPct/100`
   (default **90%**, `defaults.go:34`) the cycle is skipped and `consecutiveSkips++`.
   After `BackoffConsecutiveSkips` skips (default **3**, `defaults.go:38`) the breaker goes
   **dormant** and the ticker resets to `DormantInterval` (default **600s**, `defaults.go:39`).
   `RecordSuccess` clears dormancy after 3 consecutive good cycles (`circuit_breaker.go:66`).
   If the load query itself errors, it skips "to be safe" (`circuit_breaker.go:36`).
2. **Collect** (`collect`, `collector.go:115`) — gathers all categories (below) into a `Snapshot`.
3. Swap `previous = latest; latest = snap` under `mu` (so the analyzer can diff cycles).
4. `persist` (`collector_helpers.go:70`) — writes one `sage.snapshots` row **per category**
   (`queries`, `tables`, `indexes`, `foreign_keys`, `system`, `locks`, `sequences`,
   `replication`, `io`, `partitions`, `config_data`) inside a single transaction.

Fatal vs. non-fatal: queries/tables/indexes/FKs/system/locks/sequences are fatal (abort the
cycle on error). Replication, IO, prepared-xacts, partitions, and config are **WARN-and-continue**
(`collector.go:146-170`) so one quirk (e.g. unreserved slot on a standby) doesn't silently halt
all collection.

### 1.2 Snapshot data model (`snapshot.go`)

`Snapshot` (`snapshot.go:6`): `CollectedAt`, `Queries[]`, `Tables[]`, `Indexes[]`,
`ForeignKeys[]`, `System`, `Locks[]`, `Sequences[]`, `Replication*`, `IO[]`, `Partitions[]`,
`PreparedXacts[]`, `ConfigData*`, `StatsReset bool`.

| Category | Query (queries.go) | Struct | Notes |
|---|---|---|---|
| Query stats | `queryStatsSQL` + 3 variants (`:5-69`) | `QueryStats` (`:24`) | from `pg_stat_statements`, filtered to current `dbid`; excludes pg_sage's own queries via `NOT ILIKE '%pg_sage%'` + a `sage.`-schema regex. `ORDER BY total_exec_time DESC LIMIT MaxQueries` (default **500**). WAL/plan-time columns appended conditionally on `cfg.HasWALColumns` / `cfg.HasPlanTimeColumns` (`collector.go:187`). `blk_read_time`/`blk_write_time` are hardcoded `0` in the SELECT. |
| Table stats | `tableStatsSQL` (`:71`) | `TableStats` (`:50`) | `pg_stat_user_tables` JOIN `pg_class` + size functions. Excludes `sage,pg_catalog,information_schema,google_ml`. **Keyset-paginated** by `(schema, rel)` tuple, `BatchSize` rows/page (default **1000**) — a 2-bind tuple cursor (`collector.go:243`); single-string concat would skip rows. Captures `relpersistence` (unlogged detection). |
| Index stats | `indexStatsSQL` (`:92`) | `IndexStats` (`:84`) | `pg_stat_user_indexes` + `pg_statio_user_indexes` + `pg_index` + `pg_am`. Carries `indisunique/indisprimary/indisvalid`, `indexdef`, `index_type` (am name). |
| Foreign keys | `foreignKeysSQL` (`:109`) | `ForeignKey` (`:100`) | `pg_constraint contype='f'`, one row per FK column. |
| System | `systemStatsSQL14`/`17` (`:150/157`) | `SystemStats` (`:107`) | active/idle-in-tx/total backends, max_connections, cache hit ratio, deadlocks, blk_read/write_time, total checkpoints, `pg_is_in_recovery()`, db size. Uses `pg_stat_bgwriter` (PG14-16) vs `pg_stat_checkpointer` (PG17+) selected by `pgVersionNum`. |
| Locks | `locksSQL` (`:163`) | `LockInfo` (`:124`) | `pg_locks` LEFT JOIN `pg_class`/`pg_stat_activity`, excludes own backend. |
| Sequences | `sequencesSQL` (`:176`) | `SequenceStats` (`:138`) | `pg_sequences`, computes `pct_used = last_value/max_value*100`. |
| Replication | `replicationReplicasSQL` (`:187`), `replicationSlotsSQL` (`:199`) | `ReplicationStats` (`:150`) | `pg_stat_replication` + `pg_replication_slots`. Slot query is standby-safe (`pg_last_wal_receive_lsn` when in recovery) and COALESCEs `restart_lsn` NULL→0. Returns `nil` when no replicas/slots (`collector.go:463`). |
| IO (PG16+) | `ioStatsSQL` (`:211`) | `IOStats` (`:170`) | `pg_stat_io`, filtered to rows with activity, `LIMIT 100`. Gated on `pgVersionNum >= 160000`. |
| Partitions | `partitionInheritanceSQL` (`:226`) | `PartitionInfo` (`:189`) | `pg_inherits` child→parent map. |
| Prepared xacts | `preparedXactsSQL` (`:242`) | `PreparedTransaction` (`:244`) | `pg_prepared_xacts` with `age(transaction)` xid age. Called out as a "critical blind spot" (invisible to `pg_stat_activity`). |
| Config | `collectConfigSnapshot` (`queries_config.go:9`) | `ConfigSnapshot` (`:199`) | Only when `cfg.Advisor.Enabled`. Pulls `pg_settings` (autovacuum* + a fixed whitelist of WAL/memory/connection GUCs), per-table reloptions, connection-state distribution, current WAL LSN, available extensions (`pg_repack,pg_buffercache,pgstattuple,hypopg`), and 5-minute connection churn. |

`StatStatementsMax` is fetched separately (`collector_helpers.go:52`) from
`pg_stat_statements.max`; returns 0 if extension absent.

**Stats-reset detection** (`detectStatsReset`, `collector_helpers.go:15`): returns true only when
**both** >50% of overlapping queryids show decreased calls **and** total calls dropped >80%
(`currTotal < prevTotal/5`). Sets `snap.StatsReset`, which makes the analyzer skip query-based rules.

---

## 2. Analyzer / Rules Engine (`internal/analyzer`)

### 2.1 Cycle (`analyzer.go:202`)

Runs immediately on start, then every `cfg.Analyzer.Interval()` (default **600s**,
`defaults.go:19`). Per cycle:

1. Pull `current`/`previous` from collector; skip if no snapshot yet.
2. `filterSchemaExclusions` (`analyzer.go:178`) — drops `sage,pgsnap,pg_catalog,information_schema,google_ml`
   tables/indexes (belt-and-suspenders over the collector's SQL filter).
3. `loadRecentlyCreatedIndexes` — reads `sage.action_log` for `CREATE INDEX` in the last
   `UnusedIndexWindowDays` (default **7**) to suppress unused-index findings on freshly created indexes.
4. If `StatsReset`, skip the 3 query rules (`slow_queries`, `high_plan_time`, `high_total_time`)
   plus regression/sort/plan-diff (`analyzer.go:220`).
5. Run every `AllRules` entry (`rules.go:27`), then the DB-query-backed rules, optimizer, advisor,
   forecaster, tuner, work_mem promotion, extension drift, lock chains, and RCA.
6. `DeduplicateFindings` (`dedup.go:67`) with computed I/O-util pct.
7. Store in memory, `UpsertFindings`, dispatch critical/rewrite notifications, `ResolveCleared`.

### 2.2 Rule families

Severity vocabulary: `info` < `warning` < `critical` (`dedup.go:10`).
`ActionRisk`: `safe` / `moderate` / `high_risk`.

#### Index health (`rules_index.go`)

| Rule | Trigger | Thresholds (config key / default) | Severity | Finding category / SQL |
|---|---|---|---|---|
| `ruleUnusedIndexes` (`:52`) | `idx_scan==0`, not primary/unique/invalid, not FK-only-support, first observed ≥ window ago | `UnusedIndexWindowDays` (`unused_index_window_days`, **7**). Window tracked in-process via `extras.FirstSeen`. | `warning` (`info` if unlogged) | `unused_index`; `DROP INDEX CONCURRENTLY`; rollback = recreate from indexdef |
| `ruleInvalidIndexes` (`:126`) | `indisvalid=false` | none | `warning` (`info` if unlogged) | `invalid_index`; `DROP INDEX CONCURRENTLY` |
| `ruleDuplicateIndexes` (`:176`) | Exact-duplicate **or** subset btree (via `ParseIndexDef`/`IsDuplicate`/`IsSubset`). Constraint-backed (PK/unique) indexes are protected from being dropped. | none | `critical` | `duplicate_index`; drops the lower-`idx_scan` / non-protected one |
| `ruleMissingFKIndexes` (`:318`) | FK column not covered as a leading-prefix of any index | none | `warning` (`info` if unlogged) | `missing_fk_index`; `CREATE INDEX CONCURRENTLY`. Tables flagged here are excluded from the seq-scan watchdog. |

**Index bloat / REINDEX is NOT implemented as a rule.** `IndexBloatThresholdPct`
(`index_bloat_threshold_pct`, default **30**, `defaults.go:23`) exists as a config key and is
plumbed through the API/store, but there is **no `ruleIndexBloat`** in the analyzer — dead config.

#### Query performance

| Rule | File:line | Trigger | Thresholds | Severity | Category |
|---|---|---|---|---|---|
| `ruleSlowQueries` | `rules_query.go:12` | `mean_exec_time > threshold` | `SlowQueryThresholdMs` (`slow_query_threshold_ms`, **1000**). `warning` 1–5×, `critical` >5× | warn/crit | `slow_query` |
| `ruleHighPlanTime` | `rules_query.go:58` | `mean_plan_time > mean_exec_time` AND `calls≥100` AND ratio≥2 | ratio>10 → critical | warn/crit | `high_plan_time` |
| `ruleQueryRegression` | `rules_query.go:111` | current mean > historical avg × `(1+pct/100)`; skips if calls dropped >90% (reset guard) | `RegressionThresholdPct` (`regression_threshold_pct`, **50**), `RegressionLookbackDays` (**7**). >200% increase → critical. Historical avg built from `sage.snapshots` downsampled to ~100 (`analyzer.go:561`). | warn/crit | `query_regression` |
| `ruleSeqScanWatchdog` | `rules_query.go:182` | `seq_scan>100` AND `n_live_tup≥minRows` AND not (`idx_scan>0 && seq_scan≤idx_scan*10`) | `SeqScanMinRows` (`seq_scan_min_rows`, **100000**) | `warning` | `seq_scan_heavy` |
| `ruleTotalTimeHeavy` | `rules_total_time.go:14` | Δ`total_exec_time` between cycles > 10% of wall-clock interval | hardcoded 10% (warn), >50% of wall → critical | warn/crit | `high_total_time` |
| `ruleHighFreqFirstCycle` | `rules_total_time.go:93` | First cycle only (`previous==nil`), top-3 queries with `calls>10000` | hardcoded | `info` | `high_total_time` |
| `ruleSortWithoutIndex` | `rules_sort.go:28` | Plan JSON has Limit→Sort where `sortRows/limitRows ≥ 10` | hardcoded 10× | `warning` | `sort_without_index`. Reads `sage.explain_cache` (last 1 day). |
| `rulePlanRegression` | `rules_plan_diff.go:37` | Compares two most-recent plans/query: `cost_ratio≥2` (or ≥1.5 with node downgrade/new disk spill) | ratio≥10 critical, ≥2 warning (`severityFromCostRatio`). Detects Index→Seq Scan downgrades, new disk spills. | warn/crit | `plan_regression`. Reads `sage.explain_cache` (last 7 days). |

#### Vacuum / dead tuples / wraparound (`rules_vacuum.go`)

| Rule | Trigger | Thresholds | Severity | Category / SQL |
|---|---|---|---|---|
| `ruleTableBloat` (`:42`) | `dead/(live+dead) > threshold` AND `live+dead ≥ minRows` | `TableBloatDeadTuplePct` (`table_bloat_dead_tuple_pct`, **20**), `TableBloatMinRows` (`table_bloat_min_rows`, **1000**) | `warning` → downgraded to `info` if I/O-saturated (`ioSaturated`>50%) | `table_bloat`; `VACUUM <table>` |
| `ruleXIDWraparound` (`:113`) | `age(datfrozenxid) ≥ warning` (queried separately, `analyzer.go:511`) | `XIDWraparoundWarning` (**500M**), `XIDWraparoundCritical` (**1B**) | warn/crit | `xid_wraparound`; risk = `moderate`; SQL surfaces top xmin holders |

#### Sequence exhaustion (`rules_sequence.go:14`)

Trigger `pct_used ≥ 75`. `warning` 75–90%, `critical` ≥90%. **Thresholds hardcoded** (not config).
Recommends bigint migration for `integer` sequences. Category `sequence_exhaustion`, risk `safe`.

#### System / config audit (`rules_system.go`)

| Rule | Trigger | Thresholds | Severity | Category |
|---|---|---|---|---|
| `ruleConnectionLeaks` (`:20`) | idle-in-tx longer than timeout (queried separately, `analyzer.go:524`) | `IdleInTxTimeoutMinutes` (`idle_in_tx_timeout_minutes`, **30**) | `warning` | `connection_leak`; `pg_terminate_backend(pid)` |
| `ruleCacheHitRatio` (`:50`) | `cache_hit_ratio < warning` | `CacheHitRatioWarning` (`cache_hit_ratio_warning`, **0.95**); `<0.80` → critical | warn/crit | `cache_hit_ratio` |
| `ruleCheckpointPressure` (`:91`) | checkpoints/hr > threshold (needs prev snapshot ≥60s apart) | `CheckpointFreqWarningPerHour` (**12**) | `warning` | `checkpoint_pressure` |
| `ruleStatStatementsCapacity` (`:142`) | `len(queries)/stat_statements_max ≥ 80%` | hardcoded; >95% → critical | warn/crit | `stat_statements_pressure` |
| `checkExtensionDrift` (`rules_extension_drift.go:17`) | `extversion != default_version` | none | `warning` | `extension_drift` (advisory-only, no SQL) |
| `checkWorkMemPromotion` (`rules_workmem_promotion.go:33`) | A role has ≥ threshold active `Set(work_mem "NMB")` hints in `sage.query_hints` | `WorkMemPromotionThreshold` (`work_mem_promotion_threshold`, **5**; 0 disables) | `info` | `work_mem_promotion`; `ALTER ROLE ... SET work_mem` (risk `moderate`, advisory) |

#### Security

There is **no dedicated security rule family** in Tier 1. The closest analogues are the
remediation-style findings (`connection_leak` → terminate, `lock_chain` → cancel/terminate)
and `agent_workload` attribution (below). SQL is always built with `sanitize.QuoteQualifiedName`
/ identifier quoting (e.g. `rules_vacuum.go:102`, `rules_workmem_promotion.go:134`). No
pg_hba / role-privilege / SSL audit rule exists.

#### Replication (`rules_replication.go`)

| Rule | Trigger | Thresholds | Severity | Category |
|---|---|---|---|---|
| `ruleReplicationLag` (`:15`) | `replay_lag ≥ 30s` (`parsePGInterval`) | hardcoded 30s warn / 5min critical | warn/crit | `replication_lag` |
| `ruleInactiveSlots` (`:82`) | slot `active=false` | none | `warning` | `inactive_slot`; `pg_drop_replication_slot` |
| (same fn) slow active slots | active slot `retained_bytes ≥ threshold` | `SlowSlotRetainedBytes` (`slow_slot_retained_bytes`, **1 GiB**); ≥5× → critical | warn/crit | `slow_replication_slot` (risk `moderate`) |

#### Lock chains (`rules_lockchain.go`)

`DetectLockChains` (`:123`) runs only when `cfg.Analyzer.LockChain.Enabled`. A recursive CTE over
`pg_blocking_pids()` (depth ≤10) finds each root blocker and its blocked tree; a second query
enriches up to 5 locked relations. `lockChainFindings` (`:238`):

- **Safe process** (`isSafeProcess`: own PID, `application_name` contains `pg_sage`, or matches
  `SafePatterns` default `[pg_sage, replication, patroni]`) → always `info`, no remediation SQL.
- Below `MinBlockedThreshold` (**3**) and not safe → skipped.
- `critical` when `total_blocked ≥ CriticalBlockedThreshold` (**10**), else `warning`.
- Remediation depends on root state + duration: `idle in transaction ≥ IdleInTxTerminateMinutes`
  (**5**) → `pg_terminate_backend` (risk `moderate`); `active ≥ ActiveQueryCancelMinutes` (**15**)
  → `pg_cancel_backend` (risk `safe`); otherwise monitor-only. Category `lock_chain`.

#### Self-monitoring

There is no self-monitoring rule that *produces* findings; instead self-monitoring findings are
**filtered out** of persistence. `UpsertFindings` skips any finding for which
`selfmonitor.IsFinding` is true (`finding.go:39`, `:128`) — pg_sage's own objects never become
findings. (Self-action *correlation* lives in RCA, §4.)

#### Workload attribution (dead/unwired)

`ClassifyAgentWorkload` / `AgentWorkloadFinding` (`agent_workload.go`) classify connections as
attributed/unattributed/ephemeral and emit an `info` `agent_workload` finding. **Only referenced by
tests** — never called from the analyzer cycle. Effectively dead code.

### 2.3 Forecaster findings (wired via analyzer)

`forecastDiskGrowth`, `…ConnectionSaturation`, `…CachePressure`, `…SequenceExhaustion`,
`…QueryVolume`, `…CheckpointPressure` (see §3) plus `GrowthFindings`/`storage_forecast`. These
flow into `allFindings` through the `forecaster` interface (`analyzer.go:326`).

---

## 3. Finding lifecycle (`finding.go`)

`Finding` struct (`finding.go:15`): `Category, Severity, ObjectType, ObjectIdentifier, Title,
Detail map[string]any, Recommendation, RecommendedSQL, RollbackSQL, ActionRisk, DatabaseName`
(+ optional `RuleID`/`ImpactScore` for the schema-lint subsystem). Persisted to `sage.findings`.

**Identity** = `(Category, ObjectIdentifier)` among `status='open'`.

**Creation / dedup-on-upsert** (`UpsertFindings`, `:37`):
- Self-monitoring findings skipped (`:39`).
- If an open `(category, object_identifier)` exists → **bump** `occurrence_count`, refresh
  `last_seen`, `detail`, `severity`, `title`, `recommendation`, `recommended_sql` (and rule_id/
  impact_score when non-empty/non-zero). Titles/recs legitimately change between scans.
- Else → check **resolution-by-action grace**: `recentlyResolvedByAction` (`:138`) — if the same
  `(category, object)` was resolved by an action (`action_log_id IS NOT NULL`) within the last
  **2 minutes** (`actionResolutionReopenGrace`, `:12`), **do not re-insert** (avoids flapping right
  after the executor fixes something).
- Else → INSERT new `status='open'`, `occurrence_count=1`.

**Status transitions**: `open` → `resolved` (`resolved_at` set). Suppression (`suppressed`) and
unsuppression are driven by the REST API, not the rules engine. `ResolveCleared` (`:165`) marks
open findings of a category `resolved` when their `object_identifier` is absent from the current
cycle's active set for that category (`analyzer.go:403`).

**In-cycle dedup** (`DeduplicateFindings`, `dedup.go:67`) before persistence:
1. If I/O-util >50%, downgrade all vacuum-category findings to `info` (`:92`).
2. Group by `ObjectIdentifier`. Within a group: per-query/tuning categories (`query_tuning`,
   `plan_regression`, `*hint*`, `*tuner*`) **beat** global `*_tuning` config-advisor findings
   (`applyQueryTuningRule`, `:149`). Same category → keep highest severity, tiebreak to the one
   with `RecommendedSQL` (`pickBetter`, `:312`).
3. `resolveGUCConflicts` (`:246`): when two findings target the same GUC (parsed from
   `ALTER SYSTEM SET` / `SET` / `ALTER TABLE ... SET (...)`), per-table beats global, then higher
   severity wins; the loser is dropped.

**Notifications**: critical findings → `FindingCriticalEvent`; `query_tuning` findings with a
`suggested_rewrite` → `QueryRewriteEvent` (`analyzer.go:429`/`451`), only if a dispatcher is set.

---

## 4. Forecaster (`internal/forecaster`)

Predicts capacity/exhaustion from historical `sage.snapshots` / `sage.size_history`. Math:

- **`LinearRegression`** (`stats.go:20`) — OLS slope/intercept + R² on `(day_offset, value)`.
- **`EWMA`** (`stats.go:67`) — exponentially weighted moving average (α=0.1 in callers).
- **`DaysUntilThreshold`** (`stats.go:81`) — `(threshold−current)/slope`; `+Inf` if slope≤0.
- **`WeekOverWeekGrowthPct`** (`stats.go:97`) — last-7 mean vs prior-7 mean.
- **`linearRegression`/`forecastGrowth`** (`growth.go:40/85`) — OLS on `(unix_ts, bytes)`,
  projects `days_until_full` against `DiskCapacityBytes`.

All daily forecasters require **`minDataPoints=7`** (`rules.go:10`; query-volume needs 14;
checkpoint needs 8). Findings (`rules.go`):

| Forecast | Trigger / severity | Config |
|---|---|---|
| `forecast_disk_growth` | slope > `DiskWarnGrowthGBDay` (default **5.0** GB/day) → warning | `DiskWarnGrowthGBDay` |
| `forecast_connection_saturation` | days-to-`ConnectionWarnPct` (**80%**) via `severityByDays`: <30 critical, 30–90 warning, ≥90 none | `ConnectionWarnPct` |
| `forecast_cache_pressure` | EWMA < `CacheWarnThreshold` (**0.95**) OR declining slope with R²>0.5 → warning | `CacheWarnThreshold` |
| `forecast_sequence_exhaustion` | days-to-100% ≤ `SequenceCriticalDays` (**30**) crit / ≤ `SequenceWarnDays` (**90**) warn | per-sequence |
| `forecast_query_volume` | WoW growth >100% critical / >50% warning | needs 14 days |
| `forecast_checkpoint_pressure` | EWMA rate > 12/hr → warning | hardcoded |
| `storage_forecast` (`growth.go:231`) | `days_until_full`: ≤3 & R²≥0.8 critical, ≤7 & R²≥0.5 warning, ≤30 & R²≥0.5 info; R²<`MinRSquared` → info "unreliable" | `DiskCapacityBytes`, `MinDataPoints` (**24**), `MinRSquared` (**0.5**) |

Defaults: `defaults.go:133-158`. `ForecastGrowth` is no-op unless `MinDataPoints>0` (v0.9 fields configured).

---

## 5. RCA Engine (`internal/rca`)

Root-cause correlation running **after** each analyzer cycle (`Engine.Analyze`, `rca.go:135`).
Tier 1 = deterministic decision trees; Tier 2 = optional LLM correlation; plus self-action correlation.

### 5.1 Signals (`signals.go`)

8 detectors (`detectSignals`, `:16`):

| Signal ID | Fires when | Threshold (config / default) |
|---|---|---|
| `connections_high` | `total_backends/max_connections ≥ pct` | `RCA.ConnectionSaturationPct` (**80**); ≥90 critical |
| `idle_in_tx_elevated` | idle-in-tx avg duration ≥ timeout (from `ConfigData.ConnectionStates`) | `IdleInTxTimeoutMinutes` (fallback 5) |
| `cache_hit_ratio_drop` | `cache_hit_ratio < warning` | `CacheHitRatioWarning` (**0.95**); −0.05 → critical |
| `replication_lag_increasing` | worst replica replay lag ≥ threshold | `RCA.ReplicationLagThresholdS` (**30s**) |
| `vacuum_blocked` | any table dead% ≥ threshold | `TableBloatDeadTuplePct` (fallback 10) |
| `lock_contention` | any `lock_chain` finding passed in | from lock-chain findings |
| `wal_growth_spike` | `currWAL/prevWAL ≥ multiplier` | `RCA.WALSpikeMultiplier` (**2.0**) |
| `orphaned_prepared_tx` | `len(PreparedXacts)>0` | xid age >100M → critical |

### 5.2 Decision trees (`trees.go`) — heuristics

- **`treeConnectionsHigh`** (`:76`): if `idle_in_tx_elevated` co-fires → "idle-in-tx saturating pool"
  (moderate); elif churn `> 2× previous` → "connection storm"; else "gradual growth" (safe).
- **`treeCacheHitDrop`** (`:146`): if any query's `SharedBlksRead > 10× previous` → "query evicting
  buffers"; else "working set exceeds shared_buffers".
- **`treeVacuumBlocked`** (`:202`): idle-in-tx lock holder → terminate (high_risk); elif prepared
  xact → `ROLLBACK PREPARED` (high_risk, forced critical); else "autovacuum falling behind".
- **`treeOrphanedPreparedTx`** (`:278`): always high_risk, surfaces `ROLLBACK PREPARED '<gid>'`.
- Simple 1:1 incidents for replication/lock/WAL signals (`simpleIncident`, `:313`).

### 5.3 Tier 2 LLM (`tier2.go`)

When LLM enabled and uncovered-signal count ≥ `LLMCorrelationThreshold` (**3**), sends signals to
the LLM (`callTier2LLM`, `:74`), parses markdown-fenced JSON via `llm.ParseJSON`, builds a
`source="llm"` incident at fixed confidence **0.6**. 30s timeout.

### 5.4 Incident lifecycle (`rca.go`)

- **Dedup** (`:204`): match on `source` + sorted `SignalIDs` + first affected object within
  `DedupWindowMinutes` (**30**) → bump `OccurrenceCount`, raise severity, refresh `LastDetectedAt`.
- **Auto-resolve** (`autoResolve`, `:259`): after `gracePeriodLeft` expires, an incident whose
  signals stop firing for `ResolutionCycles` (**2**) consecutive cycles is resolved.
- **Escalate** (`escalate`, `:295`): a `warning` incident with `OccurrenceCount ≥ EscalationCycles`
  (**5**) is promoted to `critical`.
- **Self-action correlation** (`self_action.go` via `applySelfActionCorrelation`, `:325`): queries
  `sage.action_log` for recent actions (30 min) + rollback history (30 days) and flags incidents as
  self-caused / manual-review.
- **Persist** (`PersistIncidents`, `:385`): UPSERT into `sage.incidents`.

RCA defaults: `defaults.go:141-148`.

---

## 6. Dead / unwired / notable

- **`agent_workload.go`** — classifier + finding builder exist but are never invoked from the
  cycle; only tests call them. Dead.
- **Index bloat / REINDEX** — `IndexBloatThresholdPct` config key + default (30) exist and are
  API-editable, but no rule consumes them. The README/CLAUDE.md claim of "index bloat" detection is
  unbacked in Tier 1.
- **`blk_read_time`/`blk_write_time` in `pg_stat_statements`** are hardcoded to `0` in the SELECT
  (`queries.go:11`), so `ioWaitRatio` (`rules_vacuum.go:16`) relies on the per-database
  `blk_read_time`/`blk_write_time` in `SystemStats` instead (`analyzer.go:645`).
- **`parsePGInterval`** appears in two places (`rules_replication.go:187` and rca `signals.go`'s
  `parseIntervalSeconds`) — duplicate interval parsing.
- Sequence exhaustion thresholds (75/90), slow-query ratio cuts (5×), seq-scan ratio (10×),
  total-time wall-clock % (10/50), sort waste (10×), and replication lag (30s/5min) are all
  **hardcoded**, not config-driven, despite many having config-shaped siblings.
