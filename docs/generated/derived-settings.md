# Derived settings

> Generated from `internal/selfconfig` by `cmd/gen_config_meta`; do not edit manually.

pg_sage derives these keys per database when the operator leaves them unset. Every other key is never derived: see the self-config class in [configuration field lifecycles](config-lifecycles.md).

- **Evidence and bounds.** A rule reads the database (catalog size, scan times, connection limit, temp-file rate, pg_sage's own cost) and clamps its value to bounds that never widen authority or spend: past the product default only where a fixed ceiling is justified below.
- **Shadow first.** A new value is recorded in shadow and soaks for `self_config.soak_hours` (default 24). It is promoted only when the comparison against the active value's measured outcomes is not worse; otherwise it stays in shadow with the reason.
- **Restart-bound keys** change only at startup; a value promoted while running is reported as pending restart.
- **The operator always wins.** A key set in the YAML file, a fleet `databases[]`/`defaults` field or an API override is never derived and shows as set by the operator. An admin can pin the current value ("pin current") or unpin it from the Configuration page or `POST /api/v1/derived-settings/{key}/pin`; a pinned value ignores new evidence until it is unpinned.
- **Ledger.** Every derivation, promotion, hold, pin and unpin is recorded in `sage.config_derivation` with the value, previous value, cited evidence, bounds, rule and version. `self_config.enabled: false` turns derivation off.

| Key | Lifecycle | Bounds | Spends more when | Evidence | Shadow comparison |
| --- | --- | --- | --- | --- | --- |
| `collector.interval_seconds` | live | 60-600 s (default 60) | lower | measured time of the catalog scan plus the pg_stat_statements read a collector cycle makes; relation count for context. | mean measured cycle cost against the 1% budget |
| `safety.query_timeout_ms` | restart | 500-5000 ms (default 500) | higher | measured time of one pass over pg_class with its statistics; relation count. | largest catalog scan during the soak needs 2x headroom |
| `sre.runways.sequence_interval_seconds` | restart | 600-2400 s (default 600) | lower | measured time to read every sequence's last value; sequence count. | mean measured scan time against the 0.1% budget |
| `sre.detectors.temp_file_mb` | restart | 1024-65536 MiB (default 1024) | lower | pg_stat_database.temp_bytes rate (since the previous sample, or since the statistics reset) over sre.detectors.window_seconds. | share of soak windows over each threshold: raise only when >=10% cross the active one and <=5% cross the candidate |
| `sre.detectors.lwlock_waiters` | restart | 8-64 backends (default 8) | lower | the server's max_connections. | none measurable (real contention is rare); the soak is the test |

## Rules

- `collector.interval_seconds` (rule `collector_interval_by_cycle_cost`, version 1): Collector cadence that keeps one collector cycle's database time within 1% of one backend. Never derived past the default in the direction that spends more.
- `safety.query_timeout_ms` (rule `catalog_read_deadline`, version 1): Deadline of pg_sage's catalog and statistics reads, sized to 4x the measured catalog scan so a large catalog is still collected. Fixed ceiling: A deadline shorter than the catalog scan fails every cycle and wastes the work done; a longer one only lets the same read finish. Capped at 10x the default.
- `sre.runways.sequence_interval_seconds` (rule `sequence_sampling_by_cost`, version 1): Sequence runway sampling cadence that keeps reading every sequence within 0.1% of one backend. Never derived past the default in the direction that spends more.
- `sre.detectors.temp_file_mb` (rule `temp_threshold_by_workload`, version 1): Temp-file explosion threshold at 4x this database's normal temp traffic per detector window, so routine sorts and hashes are not incidents. Never derived past the default in the direction that spends more.
- `sre.detectors.lwlock_waiters` (rule `lwlock_threshold_by_connections`, version 1): LWLock contention threshold at 2% of the server's connection limit: 8 waiters is contention on a 100-connection server and noise on a 2000-connection one. Never derived past the default in the direction that spends more.
