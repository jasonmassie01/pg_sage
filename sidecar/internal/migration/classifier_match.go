package migration

import "strings"

// matchIndexRules checks CREATE INDEX without CONCURRENTLY.
func (rc *RegexClassifier) matchIndexRules(sql string) []DDLClassification {
	if reCreateIndexConcurrently.MatchString(sql) {
		return nil
	}
	if !reCreateIndex.MatchString(sql) {
		return nil
	}
	c := newClassification(rc.ruleByID("ddl_index_not_concurrent"), sql)
	c.SafeAlternative = concurrentIndexSQL(sql, c.SafeAlternative)
	fillTableFromOnClause(sql, &c)
	return []DDLClassification{c}
}

// concurrentIndexSQL rewrites the observed CREATE INDEX into its
// CONCURRENTLY form (G7-B34). If the statement contained a literal
// (redacted to '***', e.g. in a partial-index predicate) the rewrite
// would not be the same index, so the prose fallback is returned.
func concurrentIndexSQL(sql, fallback string) string {
	if strings.Contains(sql, redactedLiteral) {
		return fallback
	}
	return reCreateIndexPrefix.ReplaceAllString(
		strings.TrimSpace(sql), "CREATE ${1}INDEX CONCURRENTLY ")
}

// matchConstraintRules checks ADD CHECK, ADD FK without NOT VALID and
// ADD PRIMARY KEY / UNIQUE that builds an index inline.
func (rc *RegexClassifier) matchConstraintRules(sql string) []DDLClassification {
	var results []DDLClassification
	hasNotValid := reNotValid.MatchString(sql)
	add := func(id string) {
		c := newClassification(rc.ruleByID(id), sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}
	if reAddCheckConstraint.MatchString(sql) && !hasNotValid {
		add("ddl_constraint_not_valid")
	}
	if reAddFK.MatchString(sql) && !hasNotValid {
		add("ddl_fk_not_valid")
	}
	if reAddKey.MatchString(sql) && !reKeyUsingIndex.MatchString(sql) {
		add("ddl_add_key_builds_index")
	}
	return results
}

// matchAlterColumnRules checks SET NOT NULL and ALTER TYPE.
func (rc *RegexClassifier) matchAlterColumnRules(
	sql string, _ int,
) []DDLClassification {
	var results []DDLClassification
	if m := reSetNotNull.FindStringSubmatch(sql); m != nil && isColumnIdent(m[1]) {
		c := newClassification(rc.ruleByID("ddl_set_not_null"), sql)
		fillTableFromAlter(sql, &c)
		c.ColumnName = unquoteIdent(m[1])
		results = append(results, c)
	}
	if m := reAlterType.FindStringSubmatch(sql); m != nil && isColumnIdent(m[1]) {
		c := newClassification(rc.ruleByID("ddl_alter_type_rewrite"), sql)
		fillTableFromAlter(sql, &c)
		c.ColumnName = unquoteIdent(m[1])
		c.TargetType = strings.TrimSpace(m[2])
		results = append(results, c)
	}
	return results
}

// matchAddColumnRules checks ADD COLUMN with a volatile default, a
// generated/serial/identity column, or NOT NULL without DEFAULT.
func (rc *RegexClassifier) matchAddColumnRules(
	sql string, pgVersion int,
) []DDLClassification {
	var results []DDLClassification
	add := func(id string) {
		c := newClassification(rc.ruleByID(id), sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}
	rewrites := false
	if m := reAddColumnDefault.FindStringSubmatch(sql); m != nil && isColumnIdent(m[1]) {
		// On PG < 11 any DEFAULT causes a rewrite; on PG 11+ only
		// volatile defaults are problematic.
		rewrites = (pgVersion > 0 && pgVersion < 11) || isVolatileDefault(m[2])
	}
	if m := reAddColumnGenerated.FindStringSubmatch(sql); m != nil && isColumnIdent(m[1]) {
		rewrites = true
	}
	if rewrites {
		add("ddl_add_column_volatile_default")
	}
	m := reAddColumnNotNull.FindStringSubmatch(sql)
	if m != nil && isColumnIdent(m[1]) && !reHasDefault.MatchString(sql) &&
		pgVersion > 0 && pgVersion < 11 {
		add("ddl_add_column_not_null")
	}
	return results
}

// matchDropRules checks DROP COLUMN (COLUMN keyword optional inside
// ALTER TABLE) and DROP TABLE.
func (rc *RegexClassifier) matchDropRules(sql string) []DDLClassification {
	var results []DDLClassification
	if reAlterTable.MatchString(sql) && hasDroppedColumn(sql) {
		c := newClassification(rc.ruleByID("ddl_drop_column"), sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}
	if reDropTable.MatchString(sql) {
		c := newClassification(rc.ruleByID("ddl_drop_table"), sql)
		fillTarget(reDropTable, sql, &c)
		results = append(results, c)
	}
	return results
}

func hasDroppedColumn(sql string) bool {
	for _, m := range reDropColumn.FindAllStringSubmatch(sql, -1) {
		if m[1] != "" || isColumnIdent(m[2]) {
			return true
		}
	}
	return false
}

// matchMaintenanceRules checks REINDEX, VACUUM FULL, REFRESH, CLUSTER,
// SET TABLESPACE, and ATTACH PARTITION.
func (rc *RegexClassifier) matchMaintenanceRules(
	sql string, pgVersion int,
) []DDLClassification {
	var results []DDLClassification

	results = append(results, rc.matchReindex(sql, pgVersion)...)
	results = append(results, rc.matchVacuumFull(sql)...)
	results = append(results, rc.matchRefresh(sql)...)
	results = append(results, rc.matchCluster(sql)...)
	results = append(results, rc.matchSetTablespace(sql)...)
	results = append(results, rc.matchAttachPartition(sql)...)

	return results
}

func (rc *RegexClassifier) matchReindex(
	sql string, pgVersion int,
) []DDLClassification {
	if !reReindex.MatchString(sql) {
		return nil
	}
	if reReindexConcurrently.MatchString(sql) {
		return nil
	}
	if pgVersion > 0 && pgVersion < 12 {
		return nil // REINDEX CONCURRENTLY not available before PG12
	}
	c := newClassification(rc.ruleByID("ddl_reindex_not_concurrent"), sql)
	fillTarget(reReindexTarget, sql, &c)
	return []DDLClassification{c}
}

func (rc *RegexClassifier) matchVacuumFull(sql string) []DDLClassification {
	if !reVacuumFull.MatchString(sql) {
		return nil
	}
	c := newClassification(rc.ruleByID("ddl_vacuum_full"), sql)
	fillTarget(reVacuumTarget, sql, &c)
	return []DDLClassification{c}
}

func (rc *RegexClassifier) matchRefresh(sql string) []DDLClassification {
	if !reRefreshMatView.MatchString(sql) {
		return nil
	}
	if reRefreshConcurrently.MatchString(sql) {
		return nil
	}
	c := newClassification(rc.ruleByID("ddl_refresh_not_concurrent"), sql)
	fillTarget(reRefreshTarget, sql, &c)
	return []DDLClassification{c}
}

func (rc *RegexClassifier) matchCluster(sql string) []DDLClassification {
	if !reCluster.MatchString(sql) {
		return nil
	}
	c := newClassification(rc.ruleByID("ddl_cluster"), sql)
	fillTarget(reClusterTarget, sql, &c)
	return []DDLClassification{c}
}

func (rc *RegexClassifier) matchSetTablespace(sql string) []DDLClassification {
	if !reSetTablespace.MatchString(sql) {
		return nil
	}
	r := rc.ruleByID("ddl_set_tablespace")
	c := newClassification(r, sql)
	fillTableFromAlter(sql, &c)
	return []DDLClassification{c}
}

func (rc *RegexClassifier) matchAttachPartition(
	sql string,
) []DDLClassification {
	if !reAttachPartition.MatchString(sql) {
		return nil
	}
	r := rc.ruleByID("ddl_attach_partition_no_check")
	c := newClassification(r, sql)
	fillTableFromAlter(sql, &c)
	return []DDLClassification{c}
}

// checkLockTimeout fires if any classification of this statement
// requires ACCESS EXCLUSIVE; the caller skips it when an earlier
// statement in the batch set lock_timeout.
func (rc *RegexClassifier) checkLockTimeout(
	sql string, prior []DDLClassification,
) []DDLClassification {
	if reLockTimeout.MatchString(sql) {
		return nil
	}
	for _, c := range prior {
		if c.LockLevel == "ACCESS EXCLUSIVE" {
			r := rc.ruleByID("ddl_missing_lock_timeout")
			lt := newClassification(r, sql)
			lt.TableName = c.TableName
			lt.SchemaName = c.SchemaName
			return []DDLClassification{lt}
		}
	}
	return nil
}
