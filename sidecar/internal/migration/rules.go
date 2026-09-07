package migration

// ruleDefinition describes a single DDL safety rule used by the
// RegexClassifier. Patterns are compiled once during init.
type ruleDefinition struct {
	ID              string
	LockLevel       string
	RequiresRewrite bool
	MinPGVersion    int
	Description     string
	SafeAltTemplate string // may contain %s placeholders
}

// ruleCatalog returns the 16 rule definitions.
func ruleCatalog() []ruleDefinition {
	rules := schemaRules()
	rules = append(rules, columnRules()...)
	rules = append(rules, maintenanceRules()...)
	return rules
}

// schemaRules covers index, constraint, and table-level DDL.
func schemaRules() []ruleDefinition {
	return []ruleDefinition{
		{
			ID: "ddl_index_not_concurrent", LockLevel: "SHARE",
			Description:     "CREATE INDEX without CONCURRENTLY blocks writes",
			SafeAltTemplate: "CREATE INDEX CONCURRENTLY %s",
		},
		{
			ID: "ddl_constraint_not_valid", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "ADD CHECK without NOT VALID scans the whole table",
			SafeAltTemplate: "ADD ... NOT VALID, then VALIDATE CONSTRAINT",
		},
		{
			ID: "ddl_fk_not_valid", LockLevel: "SHARE ROW EXCLUSIVE",
			Description:     "ADD FK without NOT VALID scans the table",
			SafeAltTemplate: "ADD ... NOT VALID, then VALIDATE CONSTRAINT",
		},
		{
			ID: "ddl_drop_column", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "DROP COLUMN: verify no dependent views/functions",
			SafeAltTemplate: "Verify no dependent views/functions before dropping",
		},
		{
			ID: "ddl_drop_table", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "DROP TABLE: verify no foreign key references",
			SafeAltTemplate: "Verify no foreign key references before dropping",
		},
		{
			ID: "ddl_attach_partition_no_check", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "ATTACH PARTITION without prior CHECK scans the partition",
			SafeAltTemplate: "Add CHECK matching partition bound on child before ATTACH",
		},
	}
}

// columnRules covers ALTER COLUMN and ADD COLUMN operations.
func columnRules() []ruleDefinition {
	return []ruleDefinition{
		{
			ID: "ddl_set_not_null", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "SET NOT NULL requires full table scan to verify",
			SafeAltTemplate: "Add CHECK (col IS NOT NULL) NOT VALID, VALIDATE, then SET NOT NULL",
		},
		{
			ID: "ddl_alter_type_rewrite", LockLevel: "ACCESS EXCLUSIVE",
			RequiresRewrite: true,
			Description:     "Changing column type rewrites the entire table",
			SafeAltTemplate: "New column + trigger + backfill + swap",
		},
		{
			ID: "ddl_add_column_volatile_default", LockLevel: "ACCESS EXCLUSIVE",
			RequiresRewrite: true,
			Description:     "ADD COLUMN with volatile DEFAULT rewrites table on PG < 11",
			SafeAltTemplate: "Add nullable, backfill in batches, then add constraint",
		},
		{
			ID: "ddl_add_column_not_null", LockLevel: "ACCESS EXCLUSIVE",
			RequiresRewrite: true,
			Description:     "ADD COLUMN NOT NULL without DEFAULT rewrites on PG < 11",
			SafeAltTemplate: "PG11+: safe with DEFAULT. PG<11: add nullable, backfill",
		},
	}
}

// maintenanceRules covers REINDEX, VACUUM, REFRESH, CLUSTER, etc.
func maintenanceRules() []ruleDefinition {
	return []ruleDefinition{
		{
			ID: "ddl_reindex_not_concurrent", LockLevel: "ACCESS EXCLUSIVE",
			MinPGVersion:    12,
			Description:     "REINDEX blocks all access; use CONCURRENTLY on PG12+",
			SafeAltTemplate: "REINDEX CONCURRENTLY",
		},
		{
			ID: "ddl_vacuum_full", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "VACUUM FULL rewrites the table and blocks all access",
			SafeAltTemplate: "Use pg_repack or regular VACUUM instead",
		},
		{
			ID: "ddl_refresh_not_concurrent", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "REFRESH MATERIALIZED VIEW without CONCURRENTLY blocks reads",
			SafeAltTemplate: "REFRESH MATERIALIZED VIEW CONCURRENTLY (needs unique index)",
		},
		{
			ID: "ddl_cluster", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "CLUSTER rewrites the table and blocks all access",
			SafeAltTemplate: "Schedule during maintenance window or use pg_repack",
		},
		{
			ID: "ddl_set_tablespace", LockLevel: "ACCESS EXCLUSIVE",
			Description:     "SET TABLESPACE moves data under ACCESS EXCLUSIVE lock",
			SafeAltTemplate: "Schedule during low-traffic window",
		},
		{
			ID: "ddl_missing_lock_timeout", LockLevel: "",
			Description:     "ACCESS EXCLUSIVE DDL without SET lock_timeout risks blocking",
			SafeAltTemplate: "SET lock_timeout = '5s' before running the DDL",
		},
	}
}
