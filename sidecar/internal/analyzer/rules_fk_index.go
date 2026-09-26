package analyzer

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// fkConstraint is one foreign key regrouped from the collector's
// one-row-per-column output (G2-B08).
type fkConstraint struct {
	Name     string
	Table    string
	RefTable string
	Columns  []string
	// Schemas are the schemas holding a table with this name. The
	// collector's FK rows carry no schema, so more than one candidate
	// means the owning schema is unknown.
	Schemas []string
}

// groupForeignKeys merges per-column FK rows into constraints. Column
// order is not guaranteed by the collector, so coverage is checked with
// set semantics (any index whose leading columns are a permutation of the
// FK columns supports the FK).
func groupForeignKeys(snap *collector.Snapshot) []fkConstraint {
	schemasByTable := make(map[string][]string)
	for _, t := range snap.Tables {
		schemasByTable[t.RelName] = append(schemasByTable[t.RelName], t.SchemaName)
	}
	index := make(map[string]int)
	var out []fkConstraint
	for _, fk := range snap.ForeignKeys {
		key := fk.TableName + "\x00" + fk.ConstraintName
		i, ok := index[key]
		if !ok {
			schemas := schemasByTable[fk.TableName]
			if len(schemas) == 0 {
				schemas = []string{"public"}
			}
			i = len(out)
			index[key] = i
			out = append(out, fkConstraint{
				Name: fk.ConstraintName, Table: fk.TableName,
				RefTable: fk.ReferencedTable, Schemas: schemas,
			})
		}
		if !containsString(out[i].Columns, fk.FKColumn) {
			out[i].Columns = append(out[i].Columns, fk.FKColumn)
		}
	}
	return out
}

// ruleMissingFKIndexes flags foreign keys without a supporting index.
// FKs whose table name exists in several schemas are skipped: the
// collector does not report the FK's schema, and guessing produced
// CREATE INDEX on the wrong tenant's table (G2-B08).
func ruleMissingFKIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	unlogged := buildUnloggedSet(current)
	indexed := indexedColumnsByTable(current)
	var findings []Finding
	for _, fk := range groupForeignKeys(current) {
		if len(fk.Schemas) != 1 || isSystemSchema(fk.Schemas[0]) {
			continue
		}
		schema := fk.Schemas[0]
		covered := false
		for _, idxCols := range indexed[tableKey{schema, fk.Table}] {
			if isLeadingSet(fk.Columns, idxCols) {
				covered = true
				break
			}
		}
		if !covered {
			findings = append(findings, missingFKFinding(fk, schema, unlogged))
		}
	}
	return findings
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

func missingFKFinding(
	fk fkConstraint, schema string, unlogged map[string]bool,
) Finding {
	colList := strings.Join(fk.Columns, ",")
	ident := fmt.Sprintf("%s.%s(%s)", schema, fk.Table, colList)
	quoted := make([]string, len(fk.Columns))
	for i, c := range fk.Columns {
		quoted[i] = sanitize.QuoteIdentifier(c)
	}
	severity := "warning"
	rec := "Create index to speed up FK lookups and deletes."
	detail := map[string]any{
		"constraint":       fk.Name,
		"fk_column":        colList,
		"fk_columns":       fk.Columns,
		"referenced_table": fk.RefTable,
	}
	if unlogged[schema+"."+fk.Table] {
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
			sanitize.QuoteQualifiedName(schema, fk.Table),
			strings.Join(quoted, ", ")),
		ActionRisk: "safe",
	}
}

// buildFKRequirements maps each table to the column sets its FKs need.
// When the FK's schema is ambiguous the requirement is attributed to
// every candidate schema, so an index that might be the only FK support
// is protected from unused-index drops (fail closed).
func buildFKRequirements(
	snap *collector.Snapshot,
) map[tableKey][][]string {
	out := make(map[tableKey][][]string)
	for _, fk := range groupForeignKeys(snap) {
		for _, schema := range fk.Schemas {
			key := tableKey{schema, fk.Table}
			out[key] = append(out[key], fk.Columns)
		}
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
	all []collector.IndexStats,
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
		for _, other := range all {
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
