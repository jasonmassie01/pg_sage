package ddlrules

// formRules covers the lock and rewrite forms only the parse-tree
// classifier recognizes (migration-04): the regex patterns cannot tell
// them apart, so on a build without the SQL parser such statements are
// unrecognized and fail closed instead.
func formRules() []Rule {
	return append([]Rule{
		{
			ID: "ddl_set_logged", LockLevel: "ACCESS EXCLUSIVE", RequiresRewrite: true,
			Description: "SET LOGGED / SET UNLOGGED rewrites the whole table under " +
				"ACCESS EXCLUSIVE",
			SafeAltTemplate: "Create the table with the persistence it needs, or " +
				"schedule the change in a maintenance window",
		},
		{
			ID: "ddl_set_access_method", LockLevel: "ACCESS EXCLUSIVE", RequiresRewrite: true,
			Description: "SET ACCESS METHOD (PostgreSQL 15+) rewrites the whole table " +
				"under ACCESS EXCLUSIVE",
			SafeAltTemplate: "Copy into a new table with the access method, backfill, " +
				"then swap; or schedule a maintenance window",
		},
		{
			ID: "ddl_add_exclusion_builds_index", LockLevel: "ACCESS EXCLUSIVE",
			Description: "ADD EXCLUDE builds its index and checks every row while " +
				"holding ACCESS EXCLUSIVE",
			SafeAltTemplate: "No concurrent form exists: schedule a maintenance window, " +
				"or build the constraint on a new table and swap",
		},
		{
			ID: "ddl_detach_partition_not_concurrent", LockLevel: "ACCESS EXCLUSIVE",
			Description: "DETACH PARTITION without CONCURRENTLY takes ACCESS EXCLUSIVE " +
				"on the parent table",
			SafeAltTemplate: "ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY " +
				"(outside a transaction block)",
		},
	}, constraintFormRules()...)
}

// constraintFormRules covers the NOT NULL constraint, RENAME, CREATE
// TRIGGER and DROP INDEX forms.
func constraintFormRules() []Rule {
	return []Rule{
		{
			ID: "ddl_not_null_constraint_scan", LockLevel: "ACCESS EXCLUSIVE",
			Description: "ADD CONSTRAINT ... NOT NULL without NOT VALID scans the whole " +
				"table under ACCESS EXCLUSIVE (PostgreSQL 18+)",
			SafeAltTemplate: "ADD CONSTRAINT ... NOT NULL col NOT VALID, then " +
				"VALIDATE CONSTRAINT",
		},
		{
			ID: "ddl_rename", LockLevel: "ACCESS EXCLUSIVE",
			Description: "RENAME takes ACCESS EXCLUSIVE and breaks every query, view " +
				"and application still using the old name",
			SafeAltTemplate: "Add the new name alongside the old one (a view or a " +
				"copied column), move the callers, then drop the old name",
		},
		{
			ID: "ddl_create_trigger", LockLevel: "SHARE ROW EXCLUSIVE",
			Description: "CREATE TRIGGER takes SHARE ROW EXCLUSIVE, which blocks " +
				"writes, and adds work to every matching row change",
			SafeAltTemplate: "SET lock_timeout and create the trigger in a " +
				"low-write window",
		},
		{
			ID: "ddl_drop_index_not_concurrent", LockLevel: "ACCESS EXCLUSIVE",
			Description: "DROP INDEX without CONCURRENTLY takes ACCESS EXCLUSIVE on " +
				"the index's table",
			SafeAltTemplate: "DROP INDEX CONCURRENTLY (outside a transaction block)",
		},
	}
}
