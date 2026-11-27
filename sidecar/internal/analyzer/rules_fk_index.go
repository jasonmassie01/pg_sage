package analyzer

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ruleMissingFKIndexes flags foreign keys without a supporting index on
// their own schema's table (rules-08). The collector reports each key's
// schema and its columns in conkey order, so nothing is guessed: a key
// whose schema is unknown (a row stored before collection-04) or whose
// table is not in the snapshot gives no finding. Coverage uses set
// semantics: an index whose leading columns are any permutation of the
// key's columns supports it; the recommended index keeps conkey order.
// A key on a partitioned table is covered by a usable partitioned index
// on the parent (rules-03) or by an index on each leaf partition
// (fkTables.leaves); partitionedFKFindings says what an uncovered one
// gets. A partition's clone of its parent's key is left to the parent.
func ruleMissingFKIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	unlogged := buildUnloggedSet(current)
	indexed := indexedColumnsByTable(current)
	tables := newFKTables(current)
	parts := newPartitionLayout(current)
	parts.addParentIndexColumns(indexed)
	var findings []Finding
	for _, fk := range current.ForeignKeys {
		if fk.Schema == "" || len(fk.Columns) == 0 || isSystemSchema(fk.Schema) {
			continue
		}
		key := tableKey{fk.Schema, fk.TableName}
		if parts.inheritsKey(key, fk.Columns) || coveredBy(fk.Columns, indexed[key]) {
			continue
		}
		if parts.relkind[key] == "p" {
			findings = append(findings,
				parts.partitionedFKFindings(fk, key, tables.leaves(key), indexed, unlogged)...)
			continue
		}
		for _, leaf := range tables.leaves(key) {
			findings = append(findings, missingFKFinding(fk, key, leaf, unlogged))
		}
	}
	return findings
}

// coveredBy reports whether one of the index column lists supports cols.
func coveredBy(cols []string, indexes [][]string) bool {
	for _, idxCols := range indexes {
		if isLeadingSet(cols, idxCols) {
			return true
		}
	}
	return false
}

func indexedColumnsByTable(snap *collector.Snapshot) map[tableKey][][]string {
	indexed := make(map[tableKey][][]string)
	for _, idx := range snap.Indexes {
		if isSystemSchema(idx.SchemaName) || !idx.IsValid {
			continue
		}
		p := ParseIndexDef(idx.IndexDef)
		if p.Table == "" {
			continue
		}
		schema := p.Schema
		if schema == "" {
			schema = idx.SchemaName
		}
		key := tableKey{schema, p.Table}
		indexed[key] = append(indexed[key], p.Columns)
	}
	return indexed
}

// missingFKFinding reports fk unsupported on table (its own table, or a
// leaf partition of its partitioned table owner).
func missingFKFinding(
	fk collector.ForeignKey, owner, table tableKey, unlogged map[string]bool,
) Finding {
	schema := table.schema
	colList := strings.Join(fk.Columns, ",")
	ident := fmt.Sprintf("%s.%s(%s)", schema, table.table, colList)
	quoted := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quoted[i] = sanitize.QuoteIdentifier(c)
	}
	severity := "warning"
	rec := "Create index to speed up FK lookups and deletes."
	detail := map[string]any{
		"constraint":       fk.ConstraintName,
		"fk_column":        colList,
		"fk_columns":       fk.Columns,
		"referenced_table": fk.ReferencedTable,
	}
	if table != owner {
		detail["partition_of"] = owner.schema + "." + owner.table
	}
	if unlogged[schema+"."+table.table] {
		severity = "info"
		detail["unlogged"] = true
		rec += " (unlogged table — indexes lost on crash)"
	}
	return Finding{
		Category:         "missing_fk_index",
		Severity:         severity,
		ObjectType:       "table",
		ObjectIdentifier: ident,
		Title:            "Missing index on FK column " + ident,
		Detail:           detail,
		Recommendation:   rec,
		RecommendedSQL: fmt.Sprintf("CREATE INDEX CONCURRENTLY ON %s (%s);",
			sanitize.QuoteQualifiedName(schema, table.table),
			strings.Join(quoted, ", ")),
		ActionRisk: "safe",
	}
}

// buildFKRequirements maps each table to the column sets its FKs need,
// on the key's own schema. A key whose schema is unknown (a row stored
// before collection-04) is attributed to every schema holding a table of
// that name, so an index that might be the only FK support is protected
// from unused-index drops (fail closed). A key on a partitioned table is
// also a requirement on each of its partitions (fkTables.supportTables).
func buildFKRequirements(
	snap *collector.Snapshot,
) map[tableKey][][]string {
	var schemasByTable map[string][]string
	tables := newFKTables(snap)
	out := make(map[tableKey][][]string)
	for _, fk := range snap.ForeignKeys {
		schemas := []string{fk.Schema}
		if fk.Schema == "" {
			if schemasByTable == nil {
				schemasByTable = tableSchemas(snap)
			}
			schemas = schemasByTable[fk.TableName]
		}
		for _, schema := range schemas {
			key := tableKey{schema, fk.TableName}
			out[key] = append(out[key], fk.Columns)
			for _, leaf := range tables.supportTables(key) {
				if leaf != key {
					out[leaf] = append(out[leaf], fk.Columns)
				}
			}
		}
	}
	return out
}

// tableSchemas lists, per table name, the schemas holding such a table.
func tableSchemas(snap *collector.Snapshot) map[string][]string {
	out := make(map[string][]string)
	for _, t := range snap.Tables {
		out[t.RelName] = append(out[t.RelName], t.SchemaName)
	}
	return out
}

func indexSupportsFKRequirement(
	idx collector.IndexStats,
	requirements map[tableKey][][]string,
) bool {
	if !idx.IsValid {
		return false
	}
	p := ParseIndexDef(idx.IndexDef)
	if p.Table == "" || len(p.Columns) == 0 {
		return false
	}
	schema := p.Schema
	if schema == "" {
		schema = idx.SchemaName
	}
	for _, req := range requirements[tableKey{schema, p.Table}] {
		if isLeadingSet(req, p.Columns) {
			return true
		}
	}
	return false
}

func indexIsOnlyFKSupport(
	idx collector.IndexStats,
	byTable map[tableKey][]collector.IndexStats,
	requirements map[tableKey][][]string,
) bool {
	if !indexSupportsFKRequirement(idx, requirements) {
		return false
	}
	p := ParseIndexDef(idx.IndexDef)
	schema := p.Schema
	if schema == "" {
		schema = idx.SchemaName
	}
	for _, req := range requirements[tableKey{schema, p.Table}] {
		if !isLeadingSet(req, p.Columns) {
			continue
		}
		for _, other := range byTable[tableKey{schema, p.Table}] {
			if other.IndexRelName == idx.IndexRelName &&
				other.SchemaName == idx.SchemaName {
				continue
			}
			if indexCoversRequirement(other, req, schema, p.Table) {
				return false
			}
		}
		return true
	}
	return false
}

// indexesByTable groups indexes by the (schema, table) their definition
// names, the key indexCoversRequirement matches on, so a check looks only
// at the indexes of its own table (it scanned every index of the database
// for each unused FK-backing index before).
func indexesByTable(all []collector.IndexStats) map[tableKey][]collector.IndexStats {
	out := make(map[tableKey][]collector.IndexStats)
	for _, idx := range all {
		p := ParseIndexDef(idx.IndexDef)
		schema := p.Schema
		if schema == "" {
			schema = idx.SchemaName
		}
		key := tableKey{schema, p.Table}
		out[key] = append(out[key], idx)
	}
	return out
}

func indexCoversRequirement(
	idx collector.IndexStats, req []string, schema, table string,
) bool {
	if !idx.IsValid {
		return false
	}
	p := ParseIndexDef(idx.IndexDef)
	if p.Table != table {
		return false
	}
	pSchema := p.Schema
	if pSchema == "" {
		pSchema = idx.SchemaName
	}
	return pSchema == schema && isLeadingSet(req, p.Columns)
}

// isLeadingSet reports whether the first len(need) columns of have are
// exactly the columns in need, in any order.
func isLeadingSet(need, have []string) bool {
	if len(need) == 0 || len(need) > len(have) {
		return false
	}
	for _, c := range have[:len(need)] {
		if !containsString(need, c) {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
