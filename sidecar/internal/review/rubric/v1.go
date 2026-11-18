package rubric

import "github.com/pg-sage/sidecar/internal/provider"

// The platforms a check's facts are visible on. Self-managed servers
// expose the WAL archiver; the managed clouds with a telemetry adapter
// expose backups and the instance class.
var (
	selfManaged = []string{provider.Postgres, provider.CNPG}
	cloudHosts  = []string{provider.RDS, provider.Aurora, provider.CloudSQL, provider.AlloyDB,
		provider.Azure}
)

func finding(id, ref string, weight int) CheckSpec {
	return CheckSpec{ID: id, Source: SourceFinding, Ref: ref, Weight: weight}
}

func critical(c CheckSpec) CheckSpec { c.Critical = true; return c }

func on(c CheckSpec, platforms []string) CheckSpec { c.Platforms = platforms; return c }

func check(id string, src Source, ref string, weight int) CheckSpec {
	return CheckSpec{ID: id, Source: src, Ref: ref, Weight: weight}
}

// V1 is rubric pg_sage-rubric/1: existing producers only, no new detector.
// docs/review.md holds the same table with each check's reasoning.
func V1() Rubric {
	return Rubric{Version: Version, Dimensions: []Dimension{
		{ID: "recoverability", Title: "Recoverability", Checks: []CheckSpec{
			on(critical(finding("recoverability.wal_archive_failing", "wal_archive_failing", 3)),
				selfManaged),
			on(finding("recoverability.wal_archive_stale", "wal_archive_stale", 2), selfManaged),
			finding("recoverability.inactive_slot", "inactive_slot", 1),
			finding("recoverability.slow_replication_slot", "slow_replication_slot", 1),
			on(finding("recoverability.backup_stale", "backup_stale", 2), cloudHosts),
			on(finding("recoverability.backup_retention", "backup_retention_below_policy", 1),
				cloudHosts),
			on(finding("recoverability.pitr_window", "pitr_window_below_policy", 1), cloudHosts),
			check("recoverability.restore_drill", SourceDrill, refDrillLastPassed, 5),
		}},
		{ID: "change_safety", Title: "Change safety", Checks: []CheckSpec{
			finding("change_safety.migration_safety", "migration_safety", 2),
			check("change_safety.login_timeouts", SourcePosture, "AP-08", 3),
			check("change_safety.serial_usage", SourceLint, "lint_serial_usage", 1),
		}},
		{ID: "workload_stability", Title: "Workload and plan stability", Checks: []CheckSpec{
			finding("workload_stability.query_regression", "query_regression", 3),
			finding("workload_stability.plan_regression", "plan_regression", 3),
			finding("workload_stability.stale_statistics", "stale_statistics", 2),
			finding("workload_stability.temp_spill", "temp_spill", 1),
		}},
		{ID: "schema", Title: "Schema", Checks: schemaChecks()},
		{ID: "capacity_cost", Title: "Capacity and cost", Checks: capacityChecks()},
		{ID: "security_access", Title: "Security and access", Checks: securityChecks()},
		{ID: "configuration", Title: "Configuration", Checks: []CheckSpec{
			finding("configuration.memory_tuning", "memory_tuning", 1),
			finding("configuration.wal_tuning", "wal_tuning", 1),
			finding("configuration.connection_tuning", "connection_tuning", 1),
			finding("configuration.checkpoint_pressure", "checkpoint_pressure", 1),
			check("configuration.pending_restart", SourceSetting, refPendingRestart, 1),
		}},
		{ID: "lifecycle", Title: "Lifecycle", Checks: []CheckSpec{
			critical(check("lifecycle.server_eol", SourceSetting, refServerVersionEOL, 3)),
			finding("lifecycle.extension_drift", "extension_drift", 2),
			check("lifecycle.upgrade_readiness", SourceUpgrade, refUpgradeReadiness, 1),
		}},
	}}
}

func schemaChecks() []CheckSpec {
	return []CheckSpec{
		check("schema.int_pk", SourceLint, "lint_int_pk", 2),
		check("schema.fk_type_mismatch", SourceLint, "lint_fk_type_mismatch", 2),
		check("schema.no_primary_key", SourceLint, "lint_no_primary_key", 2),
		check("schema.timestamp_no_tz", SourceLint, "lint_timestamp_no_tz", 1),
		check("schema.duplicate_index", SourceFirstLook, "duplicate_index", 2),
		check("schema.redundant_index", SourceFirstLook, "redundant_index", 1),
		check("schema.unindexed_fk", SourceFirstLook, "unindexed_foreign_key", 2),
	}
}

func capacityChecks() []CheckSpec {
	rightsize := on(finding("capacity_cost.rightsizing", "instance_rightsize", 1), cloudHosts)
	rightsize.Requires = RequiresCostRates
	return []CheckSpec{
		check("capacity_cost.disk_growth", SourceRunway, "forecast_disk_growth", 2),
		critical(check("capacity_cost.xid_runway", SourceFirstLook, "xid_runway", 3)),
		check("capacity_cost.multixact_runway", SourceFirstLook, "multixact_runway", 2),
		check("capacity_cost.sequence_runway", SourceFirstLook, "sequence_runway", 2),
		rightsize,
	}
}

func securityChecks() []CheckSpec {
	return []CheckSpec{
		check("security_access.role_attributes", SourcePosture, "AP-01", 2),
		critical(check("security_access.rls_exposure", SourcePosture, "AP-03", 3)),
		check("security_access.definer_functions", SourcePosture, "AP-05", 1),
		check("security_access.public_create", SourcePosture, "AP-07", 2),
		check("security_access.shared_logins", SourcePosture, "AP-13", 1),
		check("security_access.public_grants", SourcePosture, "AP-15", 2),
		check("security_access.unmanaged_logins", SourcePosture, "AP-16", 1),
	}
}
