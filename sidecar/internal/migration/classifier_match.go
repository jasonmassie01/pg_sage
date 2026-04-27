package migration

// matchIndexRules checks CREATE INDEX without CONCURRENTLY.
func (rc *RegexClassifier) matchIndexRules(sql string) []DDLClassification {
	if reCreateIndexConcurrently.MatchString(sql) {
		return nil
	}
	if !reCreateIndex.MatchString(sql) {
		return nil
	}
	r := rc.ruleByID("ddl_index_not_concurrent")
	c := DDLClassification{
		RuleID:          r.ID,
		Statement:       sql,
		LockLevel:       r.LockLevel,
		RequiresRewrite: r.RequiresRewrite,
		SafeAlternative: r.SafeAltTemplate,
		Description:     r.Description,
	}
	fillTableFromOnClause(sql, &c)
	return []DDLClassification{c}
}

// matchConstraintRules checks ADD CHECK and ADD FK without NOT VALID.
func (rc *RegexClassifier) matchConstraintRules(sql string) []DDLClassification {
	var results []DDLClassification
	hasNotValid := reNotValid.MatchString(sql)

	if reAddCheckConstraint.MatchString(sql) && !hasNotValid {
		r := rc.ruleByID("ddl_constraint_not_valid")
		c := newClassification(r, sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}

	if reAddFK.MatchString(sql) && !hasNotValid {
		r := rc.ruleByID("ddl_fk_not_valid")
		c := newClassification(r, sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}
	return results
}

// matchAlterColumnRules checks SET NOT NULL and ALTER TYPE.
func (rc *RegexClassifier) matchAlterColumnRules(
	sql string, pgVersion int,
) []DDLClassification {
	var results []DDLClassification

	if reSetNotNull.MatchString(sql) {
		r := rc.ruleByID("ddl_set_not_null")
		c := newClassification(r, sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}

	if reAlterType.MatchString(sql) {
		r := rc.ruleByID("ddl_alter_type_rewrite")
		c := newClassification(r, sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}
	return results
}

// matchAddColumnRules checks ADD COLUMN with volatile default or NOT NULL.
func (rc *RegexClassifier) matchAddColumnRules(
	sql string, pgVersion int,
) []DDLClassification {
	var results []DDLClassification

	if m := reAddColumnDefault.FindStringSubmatch(sql); m != nil {
		defaultExpr := m[2]
		// On PG < 11 any DEFAULT causes a rewrite; on PG 11+ only
		// volatile defaults are problematic.
		if pgVersion > 0 && pgVersion < 11 {
			r := rc.ruleByID("ddl_add_column_volatile_default")
			c := newClassification(r, sql)
			fillTableFromAlter(sql, &c)
			results = append(results, c)
		} else if isVolatileDefault(defaultExpr) {
			r := rc.ruleByID("ddl_add_column_volatile_default")
			c := newClassification(r, sql)
			fillTableFromAlter(sql, &c)
			results = append(results, c)
		}
	}

	if reAddColumnNotNull.MatchString(sql) && !reHasDefault.MatchString(sql) {
		if pgVersion > 0 && pgVersion < 11 {
			r := rc.ruleByID("ddl_add_column_not_null")
			c := newClassification(r, sql)
			fillTableFromAlter(sql, &c)
			results = append(results, c)
		}
	}
	return results
}

// matchDropRules checks DROP COLUMN and DROP TABLE.
func (rc *RegexClassifier) matchDropRules(sql string) []DDLClassification {
	var results []DDLClassification

	if reDropColumn.MatchString(sql) {
		r := rc.ruleByID("ddl_drop_column")
		c := newClassification(r, sql)
		fillTableFromAlter(sql, &c)
		results = append(results, c)
	}

	if m := reDropTable.FindStringSubmatch(sql); m != nil {
		r := rc.ruleByID("ddl_drop_table")
		c := newClassification(r, sql)
		c.TableName = m[3]
		if m[2] != "" {
			c.SchemaName = m[2]
		}
		results = append(results, c)
	}
	return results
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
	r := rc.ruleByID("ddl_reindex_not_concurrent")
	c := newClassification(r, sql)
	return []DDLClassification{c}
}

func (rc *RegexClassifier) matchVacuumFull(sql string) []DDLClassification {
	if !reVacuumFull.MatchString(sql) {
		return nil
	}
	r := rc.ruleByID("ddl_vacuum_full")
	return []DDLClassification{newClassification(r, sql)}
}

func (rc *RegexClassifier) matchRefresh(sql string) []DDLClassification {
	if !reRefreshMatView.MatchString(sql) {
		return nil
	}
	if reRefreshConcurrently.MatchString(sql) {
		return nil
	}
	r := rc.ruleByID("ddl_refresh_not_concurrent")
	return []DDLClassification{newClassification(r, sql)}
}

func (rc *RegexClassifier) matchCluster(sql string) []DDLClassification {
	if !reCluster.MatchString(sql) {
		return nil
	}
	r := rc.ruleByID("ddl_cluster")
	return []DDLClassification{newClassification(r, sql)}
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

// checkLockTimeout fires if any prior classification requires ACCESS
// EXCLUSIVE and the SQL doesn't include SET lock_timeout.
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
