package analyzer

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// ruleMissingFKIndexes flags foreign key columns without a supporting index.
func ruleMissingFKIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	// Build set of indexed leading columns per table.
	unlogged := buildUnloggedSet(current)
	indexed := make(map[tableKey][][]string)

	for _, idx := range current.Indexes {
		if isSystemSchema(idx.SchemaName) {
			continue
		}
		if !idx.IsValid {
			continue
		}
		p := ParseIndexDef(idx.IndexDef)
		if p.Table == "" {
			continue
		}
		key := tableKey{p.Schema, p.Table}
		indexed[key] = append(indexed[key], p.Columns)
	}

	var findings []Finding
	for _, fk := range current.ForeignKeys {
		// ForeignKey has a single FKColumn.
		// Derive schema from table stats or use public as default.
		schema := "public"
		for _, t := range current.Tables {
			if t.RelName == fk.TableName {
				schema = t.SchemaName
				break
			}
		}
		if isSystemSchema(schema) {
			continue
		}

		key := tableKey{schema, fk.TableName}
		cols := []string{fk.FKColumn}

		covered := false
		for _, idxCols := range indexed[key] {
			if isLeadingPrefix(cols, idxCols) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}

		ident := fmt.Sprintf("%s.%s(%s)", schema, fk.TableName, fk.FKColumn)
		createSQL := fmt.Sprintf(
			"CREATE INDEX CONCURRENTLY ON %s (%s);",
			sanitize.QuoteQualifiedName(schema, fk.TableName),
			sanitize.QuoteIdentifier(fk.FKColumn),
		)

		ulKey := schema + "." + fk.TableName
		severity := "warning"
		rec := "Create index to speed up FK lookups and deletes."
		detail := map[string]any{
			"constraint":       fk.ConstraintName,
			"fk_column":        fk.FKColumn,
			"referenced_table": fk.ReferencedTable,
		}
		if unlogged[ulKey] {
			severity = "info"
			detail["unlogged"] = true
			rec += " (unlogged table — indexes lost on crash)"
		}

		findings = append(findings, Finding{
			Category:         "missing_fk_index",
			Severity:         severity,
			ObjectType:       "table",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Missing index on FK column %s.%s(%s)",
				schema, fk.TableName, fk.FKColumn,
			),
			Detail:         detail,
			Recommendation: rec,
			RecommendedSQL: createSQL,
			ActionRisk:     "safe",
		})
	}
	return findings
}

func buildFKRequirements(
	snap *collector.Snapshot,
) map[tableKey][][]string {
	out := make(map[tableKey][][]string)
	for _, fk := range snap.ForeignKeys {
		schema := "public"
		for _, t := range snap.Tables {
			if t.RelName == fk.TableName {
				schema = t.SchemaName
				break
			}
		}
		key := tableKey{schema, fk.TableName}
		out[key] = append(out[key], []string{fk.FKColumn})
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
	reqs := requirements[tableKey{schema, p.Table}]
	for _, req := range reqs {
		if isLeadingPrefix(req, p.Columns) {
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
	reqs := requirements[tableKey{schema, p.Table}]
	for _, req := range reqs {
		if !isLeadingPrefix(req, p.Columns) {
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
	return pSchema == schema && isLeadingPrefix(req, p.Columns)
}

func isLeadingPrefix(need, have []string) bool {
	if len(need) > len(have) {
		return false
	}
	for i, c := range need {
		if c != have[i] {
			return false
		}
	}
	return true
}
