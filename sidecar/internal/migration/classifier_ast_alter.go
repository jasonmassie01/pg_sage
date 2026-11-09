package migration

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Lock levels, weakest first, as PostgreSQL names them.
const (
	lockShareUpdateExclusive = "SHARE UPDATE EXCLUSIVE"
	lockShareRowExclusive    = "SHARE ROW EXCLUSIVE"
	lockAccessExclusive      = "ACCESS EXCLUSIVE"
)

// alterSafeLocks are the ALTER TABLE subcommands that neither scan nor
// rewrite the table, with the lock each takes (AlterTableGetLockLevel).
var alterSafeLocks = map[pg_query.AlterTableType]string{
	pg_query.AlterTableType_AT_SetStatistics:           lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_SetOptions:              lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_ResetOptions:            lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_SetRelOptions:           lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_ResetRelOptions:         lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_ValidateConstraint:      lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_ClusterOn:               lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_DropCluster:             lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_DetachPartitionFinalize: lockShareUpdateExclusive,
	pg_query.AlterTableType_AT_EnableTrig:              lockShareRowExclusive,
	pg_query.AlterTableType_AT_EnableAlwaysTrig:        lockShareRowExclusive,
	pg_query.AlterTableType_AT_EnableReplicaTrig:       lockShareRowExclusive,
	pg_query.AlterTableType_AT_DisableTrig:             lockShareRowExclusive,
	pg_query.AlterTableType_AT_EnableTrigAll:           lockShareRowExclusive,
	pg_query.AlterTableType_AT_DisableTrigAll:          lockShareRowExclusive,
	pg_query.AlterTableType_AT_EnableTrigUser:          lockShareRowExclusive,
	pg_query.AlterTableType_AT_DisableTrigUser:         lockShareRowExclusive,
	pg_query.AlterTableType_AT_DropNotNull:             lockAccessExclusive,
	pg_query.AlterTableType_AT_ColumnDefault:           lockAccessExclusive,
	pg_query.AlterTableType_AT_DropConstraint:          lockAccessExclusive,
	pg_query.AlterTableType_AT_AlterConstraint:         lockAccessExclusive,
	pg_query.AlterTableType_AT_ChangeOwner:             lockAccessExclusive,
	pg_query.AlterTableType_AT_SetStorage:              lockAccessExclusive,
	pg_query.AlterTableType_AT_SetCompression:          lockAccessExclusive,
	pg_query.AlterTableType_AT_ReplicaIdentity:         lockAccessExclusive,
	pg_query.AlterTableType_AT_EnableRowSecurity:       lockAccessExclusive,
	pg_query.AlterTableType_AT_DisableRowSecurity:      lockAccessExclusive,
	pg_query.AlterTableType_AT_ForceRowSecurity:        lockAccessExclusive,
	pg_query.AlterTableType_AT_NoForceRowSecurity:      lockAccessExclusive,
	pg_query.AlterTableType_AT_AddIdentity:             lockAccessExclusive,
	pg_query.AlterTableType_AT_SetIdentity:             lockAccessExclusive,
	pg_query.AlterTableType_AT_DropIdentity:            lockAccessExclusive,
	pg_query.AlterTableType_AT_DropExpression:          lockAccessExclusive,
	pg_query.AlterTableType_AT_AddInherit:              lockAccessExclusive,
	pg_query.AlterTableType_AT_DropInherit:             lockAccessExclusive,
}

// alterRuleIDs are the subcommands that always match one rule.
var alterRuleIDs = map[pg_query.AlterTableType]string{
	pg_query.AlterTableType_AT_DropColumn:      "ddl_drop_column",
	pg_query.AlterTableType_AT_SetTableSpace:   "ddl_set_tablespace",
	pg_query.AlterTableType_AT_AttachPartition: "ddl_attach_partition_no_check",
	pg_query.AlterTableType_AT_SetLogged:       "ddl_set_logged",
	pg_query.AlterTableType_AT_SetUnLogged:     "ddl_set_logged",
}

// alterTable maps every subcommand. The statement holds the strongest
// lock of its subcommands, so each rule reports at least that lock. The
// table is named only for ALTER TABLE, as the regex rules do. Two type
// changes share one classification (a rule matches a statement once), so
// it names no target type: refineAlterType must not clear a rewrite on
// the first column's type alone.
func (m astMapper) alterTable(s *pg_query.AlterTableStmt) ([]DDLClassification, bool) {
	var found []DDLClassification
	known, lock, typeChanges := true, "", 0
	for _, n := range s.GetCmds() {
		cmd := n.GetAlterTableCmd()
		if cmd.GetSubtype() == pg_query.AlterTableType_AT_AlterColumnType {
			typeChanges++
		}
		hits, cmdLock, ok := m.alterCmd(cmd)
		known = known && ok
		lock = strongerLock(lock, cmdLock)
		found = mergeRules(found, hits)
	}
	rv := s.GetRelation()
	if s.GetObjtype() != pg_query.ObjectType_OBJECT_TABLE {
		rv = nil
	}
	for i := range found {
		found[i].LockLevel = strongerLock(found[i].LockLevel, lock)
		if typeChanges > 1 {
			found[i].TargetType = ""
		}
		if rv != nil {
			found[i].SchemaName, found[i].TableName = rv.GetSchemaname(), rv.GetRelname()
		}
	}
	return found, known
}

// alterCmd returns one subcommand's rules, its lock and whether it is
// known. An unknown subcommand takes ACCESS EXCLUSIVE (the default).
func (m astMapper) alterCmd(cmd *pg_query.AlterTableCmd) ([]DDLClassification, string, bool) {
	sub := cmd.GetSubtype()
	if id, ok := alterRuleIDs[sub]; ok {
		return []DDLClassification{m.rule(id, nil)}, lockAccessExclusive, true
	}
	if lock, ok := alterSafeLocks[sub]; ok {
		return nil, lock, true
	}
	switch sub {
	case pg_query.AlterTableType_AT_AddColumn:
		hits, ok := m.addColumn(cmd.GetDef().GetColumnDef())
		return hits, lockAccessExclusive, ok
	case pg_query.AlterTableType_AT_AddConstraint:
		return m.addConstraint(cmd.GetDef().GetConstraint())
	case pg_query.AlterTableType_AT_AlterColumnType:
		c := m.rule("ddl_alter_type_rewrite", nil)
		c.ColumnName = cmd.GetName()
		// A USING expression (the ColumnDef's raw default here) can compute
		// any value, and a COLLATE clause that changes the collation
		// rebuilds the column's indexes, so either stays a rewrite: no
		// TargetType to refine.
		def := cmd.GetDef().GetColumnDef()
		if def.GetRawDefault() == nil && def.GetCollClause() == nil {
			c.TargetType = typeText(def.GetTypeName())
		}
		return []DDLClassification{c}, lockAccessExclusive, true
	case pg_query.AlterTableType_AT_SetNotNull:
		c := m.rule("ddl_set_not_null", nil)
		c.ColumnName = cmd.GetName()
		return []DDLClassification{c}, lockAccessExclusive, true
	case pg_query.AlterTableType_AT_DetachPartition:
		if cmd.GetDef().GetPartitionCmd().GetConcurrent() {
			return nil, lockShareUpdateExclusive, true
		}
		return []DDLClassification{m.rule("ddl_detach_partition_not_concurrent", nil)},
			lockAccessExclusive, true
	case pg_query.AlterTableType_AT_SetAccessMethod:
		if !m.atLeast(15) {
			return nil, lockAccessExclusive, false // a syntax error before PG 15
		}
		return []DDLClassification{m.rule("ddl_set_access_method", nil)},
			lockAccessExclusive, true
	}
	return nil, lockAccessExclusive, false
}

// addColumn maps ADD COLUMN: a volatile default, serial, identity or a
// stored generated column rewrites; an inline key, check or reference
// builds an index or scans. Only a built-in type is a known safe form: a
// domain can carry a CHECK, which makes ADD COLUMN rewrite.
func (m astMapper) addColumn(def *pg_query.ColumnDef) ([]DDLClassification, bool) {
	if def == nil {
		return nil, false
	}
	var found []DDLClassification
	add := func(id string) { found = mergeRules(found, []DDLClassification{m.rule(id, nil)}) }
	known := builtinType(def.GetTypeName())
	if isSerialType(def.GetTypeName()) {
		known = true
		add("ddl_add_column_volatile_default")
	}
	if volatileExpr(def.GetRawDefault()) {
		add("ddl_add_column_volatile_default")
	}
	for _, n := range def.GetConstraints() {
		id, ok := m.columnConstraintRule(n.GetConstraint())
		known = known && ok
		if id != "" {
			add(id)
		}
	}
	return found, known
}

// columnConstraintRule is the rule an inline column constraint matches
// ("" for none) and whether the constraint is known.
func (m astMapper) columnConstraintRule(c *pg_query.Constraint) (string, bool) {
	switch c.GetContype() {
	case pg_query.ConstrType_CONSTR_DEFAULT:
		if volatileExpr(c.GetRawExpr()) {
			return "ddl_add_column_volatile_default", true
		}
		return "", true
	case pg_query.ConstrType_CONSTR_IDENTITY, pg_query.ConstrType_CONSTR_GENERATED:
		return "ddl_add_column_volatile_default", true
	case pg_query.ConstrType_CONSTR_CHECK:
		return "ddl_constraint_not_valid", true
	case pg_query.ConstrType_CONSTR_PRIMARY, pg_query.ConstrType_CONSTR_UNIQUE:
		return "ddl_add_key_builds_index", true
	case pg_query.ConstrType_CONSTR_FOREIGN:
		return "ddl_fk_not_valid", true
	case pg_query.ConstrType_CONSTR_NULL, pg_query.ConstrType_CONSTR_NOTNULL,
		pg_query.ConstrType_CONSTR_ATTR_DEFERRABLE, pg_query.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE,
		pg_query.ConstrType_CONSTR_ATTR_DEFERRED, pg_query.ConstrType_CONSTR_ATTR_IMMEDIATE:
		return "", true
	}
	return "", false
}

// addConstraint maps ADD [CONSTRAINT n] ...: NOT VALID skips the scan of
// CHECK, FOREIGN KEY and (PG 18) NOT NULL; USING INDEX attaches an
// existing index instead of building one.
func (m astMapper) addConstraint(c *pg_query.Constraint) ([]DDLClassification, string, bool) {
	one := func(id string) []DDLClassification { return []DDLClassification{m.rule(id, nil)} }
	switch c.GetContype() {
	case pg_query.ConstrType_CONSTR_CHECK:
		if c.GetSkipValidation() {
			return nil, lockAccessExclusive, true
		}
		return one("ddl_constraint_not_valid"), lockAccessExclusive, true
	case pg_query.ConstrType_CONSTR_FOREIGN:
		if c.GetSkipValidation() {
			return nil, lockShareRowExclusive, true
		}
		return one("ddl_fk_not_valid"), lockShareRowExclusive, true
	case pg_query.ConstrType_CONSTR_PRIMARY, pg_query.ConstrType_CONSTR_UNIQUE:
		if c.GetIndexname() != "" {
			return nil, lockAccessExclusive, true
		}
		return one("ddl_add_key_builds_index"), lockAccessExclusive, true
	case pg_query.ConstrType_CONSTR_EXCLUSION:
		return one("ddl_add_exclusion_builds_index"), lockAccessExclusive, true
	case pg_query.ConstrType_CONSTR_NOTNULL:
		found, ok := m.notNullConstraint(c.GetSkipValidation(), stringItems(c.GetKeys()))
		return found, lockAccessExclusive, ok
	}
	return nil, lockAccessExclusive, false
}

// notNullConstraint maps PG 18's ADD CONSTRAINT n NOT NULL col: before
// PG 18 the server rejects it, so it is unrecognized there.
func (m astMapper) notNullConstraint(notValid bool, keys []string) ([]DDLClassification, bool) {
	if !m.atLeast(18) {
		return nil, false
	}
	if notValid {
		return nil, true
	}
	c := m.rule("ddl_not_null_constraint_scan", nil)
	if len(keys) > 0 {
		c.ColumnName = keys[0]
	}
	return []DDLClassification{c}, true
}
