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
func (rc *RegexClassifier) matchAlterColumnRules(sql string) []DDLClassification {
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
		// A USING expression can compute any value, a COLLATE clause
		// rebuilds the column's indexes, and a second type change is not
		// the one this match names: each stays a rewrite, with no
		// TargetType for refineAlterType to clear it with.
		if !reUsingOrCollate.MatchString(m[0]) &&
			len(reAlterType.FindAllStringIndex(sql, 2)) == 1 {
			c.TargetType = strings.TrimSpace(m[2])
		}
		results = append(results, c)
	}
	return results
}

// matchAddColumnRules checks ADD COLUMN with a volatile default or a
// generated/serial/identity column. A constant DEFAULT is a catalog-only
// change on PostgreSQL 11+, so it is not flagged.
func (rc *RegexClassifier) matchAddColumnRules(sql string) []DDLClassification {
	if !addColumnRewrites(sql) {
		return nil
	}
	c := newClassification(rc.ruleByID("ddl_add_column_volatile_default"), sql)
	fillTableFromAlter(sql, &c)
	return []DDLClassification{c}
}

// addColumnRewrites reports whether any ADD COLUMN clause rewrites the
// table. Each ADD is read on its own, so an ADD CONSTRAINT or a constant
// default in another clause cannot hide a volatile one (the parse-tree
// mapping checks every subcommand too; env-cgo-b).
func addColumnRewrites(sql string) bool {
	for _, loc := range reAddHead.FindAllStringIndex(sql, -1) {
		rest := sql[loc[0]:]
		if m := reAddColumnDefault.FindStringSubmatch(rest); m != nil &&
			isColumnIdent(m[1]) && isVolatileDefault(m[2]) {
			return true
		}
		if m := reAddColumnGenerated.FindStringSubmatch(rest); m != nil && isColumnIdent(m[1]) {
			return true
		}
	}
	return false
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
func (rc *RegexClassifier) matchMaintenanceRules(sql string) []DDLClassification {
	var results []DDLClassification

	results = append(results, rc.matchReindex(sql)...)
	results = append(results, rc.matchVacuumFull(sql)...)
	results = append(results, rc.matchRefresh(sql)...)
	results = append(results, rc.matchCluster(sql)...)
	results = append(results, rc.matchSetTablespace(sql)...)
	results = append(results, rc.matchAttachPartition(sql)...)

	return results
}

func (rc *RegexClassifier) matchReindex(sql string) []DDLClassification {
	if !reReindex.MatchString(sql) || reReindexConcurrently.MatchString(sql) {
		return nil
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
