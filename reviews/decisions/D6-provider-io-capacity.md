# D6 (G1-B27) — Provider disk/WAL IO as capacity evidence

**Current state:** load admission requires CPU, data-IO and WAL-IO utilization. No source in the codebase ever produces the two IO values. So autonomous `CREATE INDEX` and custodian FK-index proposals are withheld on **every** deployment, self-managed included. Supabase is not an exception, even with a token.
**Recommendation:** Option C: keep fail-closed as the default. Add a per-database opt-in where the operator declares IO capacity (IOPS/MB/s). Utilization is then pg-side rate ÷ declared capacity. Surface the withheld reason in the API and dashboard now.
**Door type:** two-way. The opt-in is config, off by default, with no stored-data change. Enabling it loosens a safety gate, so it must require an explicit per-database operator action.

Worktree HEAD `f99a302`. All paths are relative to `sidecar/`.

## 1. What the code does today

- **Admission gate.**
  - `verify.Engine.OKToApplyNow` reads `source.CurrentLoad`. Any error means `load_unavailable`. It then checks CPU, data-IO and log-IO against the ceilings (`internal/verify/load.go:15-39`).
  - The error text tells the operator to use reviewed manual execution (`load.go:10-13`).
  - Default ceilings are 70/70/70 (`verify/types.go:117`). `Safety.CPUCeilingPct` overrides **all three** with one knob (`internal/executor/index_verification_runtime.go:211-214`), and that knob defaults to 90 (`config/defaults.go:40`).
- **Default source.**
  - `PostgresObservationSource.CurrentLoad` always returns `ErrLoadTelemetryUnavailable`. By design, catalogs cannot give host utilization (`verify/postgres.go:143-153`).
  - The executor substitutes a `HostLoadReader` if one is installed (`executor/host_load.go:27-37`).
- **Only HostLoadReader: Supabase.**
  - `startProviderObservability` installs `providerobs.Runtime` only when the host resolves to a Supabase project ref and `SAGE_SUPABASE_OBSERVABILITY_TOKEN` is set (`cmd/pg_sage_sidecar/provider_observability.go:37-47,67-86`).
  - It is wired in standalone (`main.go:755`), YAML fleet (`main.go:1538`) and meta (`metadb.go:489`).
  - It polls every 60 s (`providerobs/runtime.go:41-59`) from the Supabase analytics metrics endpoint (`providerobs/supabase.go:38-41,73-77`).
  - It parses only `node_time_seconds`, `node_cpu_seconds_total{mode="idle"}` and `node_memory_*` (`providerobs/metrics.go:60-63`).
  - `DeriveTelemetry` sets only `CPUPct`. It deliberately leaves Data/Log IO nil (`providerobs/telemetry.go:46-67`).
  - `Load()` rejects any nil field (`telemetry.go:37-42`), and a test pins this (`providerobs/telemetry_test.go:20-40`). Result: Supabase `CurrentLoad` always errors (G1-B27, `group-01-collection.md:57`).
- **Other load signals, not used for admission.**
  - The collector circuit breaker uses `active_backends/max_connections` (`internal/collector/circuit_breaker.go:26-31`).
  - `pg_stat_io` (PG16+) is collected into snapshots only (`collector/collector.go:153-159`, `collector/queries.go:233-246`).
  - `pg_stat_wal` is not collected anywhere: no match in `internal/` or `cmd/`.
- **API and dashboard.**
  - No endpoint exposes admission state.
  - The only trace is a `failed` `action_log` row with the error as `rollback_reason`, written on **every** cycle (`executor/executor.go:772-782`, `actionOutcome` `:1232-1236`), plus a log line.
  - `web/src` has no reference to `load_unavailable` or host load.

## 2. Signals per provider

The provider-native metric names in the last column come from vendor documentation. Nothing in this repo reads them, so they are not verified here.

| Provider | Adapter (`internal/fleet/provider_adapter.go`) | Host CPU collected | Disk/WAL IO collected | Available but unused |
|---|---|---|---|---|
| Self-managed | `postgres`, logs "local" (`:70-79`) | no | no | node_exporter on host; `pg_stat_io`/`pg_stat_wal` rates |
| RDS / Aurora | `cloudwatch` logs only (`:29-54`) | no | no | CloudWatch Read/WriteIOPS and throughput (counts, not utilization %) |
| Cloud SQL / AlloyDB | provider logging (`:16-28,55-67`) | no | no | Cloud Monitoring disk ops/bytes (counts) |
| Azure Flexible | `azure_monitor` (`:91-108`); runtime only manages server params (`cmd/pg_sage_sidecar/azure_runtime.go:19,62`) | no | no | Azure Monitor "IOPS/bandwidth consumed %" (a true utilization %) |
| Neon | `provider_console` (`:110-120`) | no | no | none via SQL; pg-side rates only |
| Supabase (token set) | `provider_console`, obs runtime | **yes**, CPU% (`telemetry.go:56-66`) | **no** (parser drops disk series, `metrics.go:60-63`) | node_exporter `node_disk_*` (per `fixes-collection.md:29`; endpoint unverified, G1-B28) |

**Blocked because of missing evidence (every provider).**
1. **Autonomous verified CREATE INDEX.** Path: `executeFinding` → `admitVerifiedCreate` → `indexVerification.Admit` (`executor/executor.go:772-782`, `executor/verified_index_identity.go:179-191`, `executor/verified_index_lifecycle.go:67-79`).
2. **Custodian verified index proposals, for example FK supporting indexes.** Path: `schemaguard.RouteVerifyIndex` → `RouteVerifiedIndex` → `SubmitVerifiedIndexProposal` → `Admit` (`internal/autonomy/schema_postgres.go:238-255`, `cmd/pg_sage_sidecar/autonomy_runtime.go:56-64`, `executor/custodian.go:25-45`).

**Not blocked.**
- Manual/approved CREATE INDEX (`executor/manual.go:74-80`). Nothing on that path calls `Admit`.
- vacuum, analyze, reindex, drop and GUC actions. They never call `OKToApplyNow`.

The gate therefore removes the flagship autonomous feature while leaving comparable IO-heavy work (`REINDEX CONCURRENTLY`, `VACUUM`) ungated. That asymmetry is itself a finding.

## 3. Options

**A. Status quo plus visibility.**
- Keep fail-closed everywhere.
- Add an admission-status field per database to the API and dashboard.
- Record a withheld decision once per finding instead of a `failed` action every cycle.
- Honest, but autonomous indexing stays off for 100% of users. **Two-way.**

**B. Treat provider busy-time or IO counters as capacity.**
- Derive IO% from `rate(node_disk_io_time_seconds_total)` (Supabase), or from provider-native metrics.
- Busy-time is not capacity on SSD or network storage. A volume at 100% "busy" can have headroom, and a throttled EBS/PD volume can sit below 100%.
- Only Azure exposes a true consumed-% metric.
- Requires N cloud integrations with credentials, which widens the security posture. **Two-way in code**, but it adds cloud credential scope.

**C. Declared capacity plus pg-side rates (recommended).**
- A per-database config `verify.io_capacity: {read_write_iops, read_write_mbps, wal_mbps}` is set by the operator from their provisioned tier.
- Utilization = Δ`pg_stat_io` (reads+writes × block size) and Δ`pg_stat_wal.wal_bytes` over a 1–2 min interval, ÷ the declared capacity.
- CPU still comes from a host reader when available. For CPU-less providers, a second explicit opt-in `verify.cpu_evidence: none_accepted` narrows the ceilings. With neither opt-in, behavior matches today.
- Works on every provider with PG16+ (`pg_stat_io`) and PG14+ (`pg_stat_wal`), with no cloud credentials. The evidence stays honest because capacity is an operator attestation, which is recorded in the decision ledger. **Two-way door**: config only, no schema semantics change, default unchanged.

Recommend **C**, with the visibility part of **A** shipped first. Revisit **B** only for Azure's consumed-% metric, as an optional higher-fidelity source.

## 4. Implementation sketch (C)

1. `internal/config/config.go`
   - Add `Verify.IOCapacity` and `Verify.CPUEvidence` per database. Validation: all values must be > 0 when set; unknown keys are rejected.
   - Split the IO ceilings off the CPU knob: add `Safety.IOCeilingPct` so it no longer inherits `CPUCeilingPct` at `index_verification_runtime.go:211-214`.
2. New `internal/verify/pgrate_load.go`
   - A `PGRateLoadReader` that samples `pg_stat_io` and `pg_stat_wal` at two points 60 s apart, keeping the previous sample like `providerobs.Runtime`.
   - Rejects counter resets (`stats_reset` changed), intervals under 1 s or over 120 s, and PG<16. It returns `ErrLoadTelemetryUnavailable` with a specific reason.
3. New `executor/host_load.go` composite reader: CPU from `providerobs.Runtime` (if any) plus IO from `PGRateLoadReader`. It is installed only when `IOCapacity` is set, in the same three wiring sites (`main.go:755,1538`, `metadb.go:489`).
4. Decision ledger: put the declared capacity and the measured values into the decision evidence (`executor/standing_policy.go:84-95` builds the ledger input). An admitted index then proves which attestation it relied on.
5. Visibility (A):
   - Add `GET /api/v1/databases/{name}/admission`, returning `{ok, reason, cpu_pct, data_io_pct, log_io_pct, sources}`.
   - Add a Value/Actions banner in `web/src`.
   - Stop the per-cycle `failed` row at `executor.go:776-779`: note the object in `recentActions` and record a single withheld decision.

No schema migration is needed. The evidence goes into existing `sage.decision` JSON.

## 5. Test plan (write first; each fails on HEAD)

- **T1 `TestPGRateLoadDerivesUtilizationFromDeclaredCapacity`**: two `pg_stat_io`/`pg_stat_wal` fixtures 60 s apart, with declared capacity 1000 IOPS. Expect `DataIOPct` = observed IOPS/10 and `LogIOPct` = wal MB/s ÷ declared.
- **T2 `TestPGRateLoadRejectsResetAndShortInterval`**: `stats_reset` changed, or interval under 1 s. Expect `ErrLoadTelemetryUnavailable` with no zero-valued sample.
- **T3 `TestAdmissionUnchangedWithoutIOCapacity`**: a regression pin that no config means `load_unavailable`. This one passes today and must keep passing.
- **T4 `TestSupabaseCPUPlusDeclaredIOAdmits`**: Supabase runtime CPU 25% plus declared IO at 10%. Expect `Admit` OK. Fails today: DataIO is nil.
- **T5 `TestIOCeilingIndependentOfCPUCeiling`**: `Safety.CPUCeilingPct=90`, `IOCeilingPct=60`, IO at 65%. Expect `data_io_ceiling`. Fails today: the ceiling is 90.
- **T6 `TestWithheldCreateIndexLogsOnce`**: three cycles with load unavailable. Expect exactly one withheld record, not three `failed` action rows.
- **T7 `TestAdmissionEndpointReportsReason`**: GET admission returns `load_unavailable` and lists the missing fields.
- **T8 `TestAdmittedDecisionRecordsCapacityAttestation`**: the decision evidence contains the declared capacity and the measured values.
- **T9 (integration, PG16 docker)**: run pgbench load, then check that the measured `DataIOPct` rises above an idle baseline and that admission flips to `data_io_ceiling`.
