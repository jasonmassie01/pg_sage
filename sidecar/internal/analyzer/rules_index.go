package analyzer

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// buildUnloggedSet returns a set of "schema.table" keys for unlogged
// tables. Indexes on unlogged tables are lost on crash, so findings
// about them should be downgraded to informational.
func buildUnloggedSet(snap *collector.Snapshot) map[string]bool {
	s := make(map[string]bool)
	for _, t := range snap.Tables {
		if t.Relpersistence == "u" {
			key := t.SchemaName + "." + t.RelName
			s[key] = true
		}
	}
	return s
}

// extractIndexNameFromSQL parses a CREATE INDEX statement and returns
// just the index name (without schema), matching IndexRelName format.
func extractIndexNameFromSQL(sql string) string {
	fields := strings.Fields(sql)
	for i, f := range fields {
		if strings.EqualFold(f, "ON") && i > 0 {
			name := fields[i-1]
			// Strip schema prefix (schema.name -> name)
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				name = name[dot+1:]
			}
			return name
		}
	}
	return ""
}

// ruleUnusedIndexes flags indexes with zero scans that are not primary keys,
// not unique, and have been observed longer than the configured window.
func ruleUnusedIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	cfg *config.Config,
	extras *RuleExtras,
) []Finding {
	window := time.Duration(cfg.Analyzer.UnusedIndexWindowDays) * 24 * time.Hour
	now := time.Now()
	unlogged := buildUnloggedSet(current)
	var findings []Finding

	for _, idx := range current.Indexes {
		if idx.IdxScan > 0 || idx.IsPrimary || idx.IsUnique || !idx.IsValid {
			continue
		}

		// Skip indexes recently created by the executor.
		if _, ok := extras.RecentlyCreated[idx.IndexRelName]; ok {
			continue
		}

		ident := idx.SchemaName + "." + idx.IndexRelName
		first, ok := extras.FirstSeen[ident]
		if !ok {
			extras.FirstSeen[ident] = now
			continue
		}
		if now.Sub(first) < window {
			continue
		}

		dropSQL := fmt.Sprintf(
			"DROP INDEX CONCURRENTLY %s.%s;",
			idx.SchemaName, idx.IndexRelName,
		)

		tableKey := idx.SchemaName + "." + idx.RelName
		severity := "warning"
		rec := "Drop unused index to save disk and write overhead."
		detail := map[string]any{
			"table":     idx.RelName,
			"index_def": idx.IndexDef,
			"size":      idx.IndexBytes,
		}
		if unlogged[tableKey] {
			severity = "info"
			detail["unlogged"] = true
			rec += " (unlogged table — indexes lost on crash)"
		}

		findings = append(findings, Finding{
			Category:         "unused_index",
			Severity:         severity,
			ObjectType:       "index",
			ObjectIdentifier: ident,
			Title: fmt.Sprintf(
				"Unused index %s (0 scans for %d+ days)",
				ident, cfg.Analyzer.UnusedIndexWindowDays,
			),
			Detail:         detail,
			Recommendation: rec,
			RecommendedSQL: dropSQL,
			RollbackSQL:    idx.IndexDef + ";",
			ActionRisk:     "safe",
		})
	}
	return findings
}

// ruleInvalidIndexes flags indexes where IsValid is false.
func ruleInvalidIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	unlogged := buildUnloggedSet(current)
	var findings []Finding
	for _, idx := range current.Indexes {
		if idx.IsValid {
			continue
		}
		ident := idx.SchemaName + "." + idx.IndexRelName
		tableKey := idx.SchemaName + "." + idx.RelName
		severity := "warning"
		rec := "Drop the invalid index and recreate if needed."
		detail := map[string]any{
			"table":     idx.RelName,
			"index_def": idx.IndexDef,
		}
		if unlogged[tableKey] {
			severity = "info"
			detail["unlogged"] = true
			rec += " (unlogged table — indexes lost on crash)"
		}
		findings = append(findings, Finding{
			Category:         "invalid_index",
			Severity:         severity,
			ObjectType:       "index",
			ObjectIdentifier: ident,
			Title:            fmt.Sprintf("Invalid index %s", ident),
			Detail:           detail,
			Recommendation:   rec,
			RecommendedSQL: fmt.Sprintf(
				"DROP INDEX CONCURRENTLY %s.%s;",
				idx.SchemaName, idx.IndexRelName,
			),
			RollbackSQL: idx.IndexDef + ";",
			ActionRisk:  "safe",
		})
	}
	return findings
}

// ruleDuplicateIndexes detects exact-duplicate and subset btree indexes.
func ruleDuplicateIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	type parsed struct {
		info   collector.IndexStats
		parsed ParsedIndex
	}
	var btrees []parsed
	for _, idx := range current.Indexes {
		if !idx.IsValid {
			continue
		}
		p := ParseIndexDef(idx.IndexDef)
		if p.IndexType != "btree" {
			continue
		}
		btrees = append(btrees, parsed{info: idx, parsed: p})
	}

	seen := make(map[string]bool)
	var findings []Finding

	for i := 0; i < len(btrees); i++ {
		for j := i + 1; j < len(btrees); j++ {
			a, b := btrees[i], btrees[j]
			aIdent := a.info.SchemaName + "." + a.info.IndexRelName
			bIdent := b.info.SchemaName + "." + b.info.IndexRelName

			if IsDuplicate(a.parsed, b.parsed) {
				drop, keep := a, b
				dropIdent, keepIdent := aIdent, bIdent
				if a.info.IdxScan > b.info.IdxScan {
					drop, keep = b, a
					dropIdent, keepIdent = bIdent, aIdent
				}
				if seen[dropIdent] {
					continue
				}
				seen[dropIdent] = true

				findings = append(findings, Finding{
					Category:         "duplicate_index",
					Severity:         "critical",
					ObjectType:       "index",
					ObjectIdentifier: dropIdent,
					Title: fmt.Sprintf(
						"Duplicate index %s (same as %s)",
						dropIdent, keepIdent,
					),
					Detail: map[string]any{
						"drop_index": dropIdent,
						"keep_index": keepIdent,
						"drop_def":   drop.info.IndexDef,
						"keep_def":   keep.info.IndexDef,
					},
					Recommendation: "Drop the duplicate index.",
					RecommendedSQL: fmt.Sprintf(
						"DROP INDEX CONCURRENTLY %s;", dropIdent,
					),
					RollbackSQL: drop.info.IndexDef + ";",
					ActionRisk:  "safe",
				})
			} else if IsSubset(a.parsed, b.parsed) {
				if seen[aIdent] {
					continue
				}
				seen[aIdent] = true
				findings = append(findings, subsetFinding(
					a.info, b.info, aIdent, bIdent,
				))
			} else if IsSubset(b.parsed, a.parsed) {
				if seen[bIdent] {
					continue
				}
				seen[bIdent] = true
				findings = append(findings, subsetFinding(
					b.info, a.info, bIdent, aIdent,
				))
			}
		}
	}
	return findings
}

func subsetFinding(
	sub, sup collector.IndexStats,
	subIdent, supIdent string,
) Finding {
	return Finding{
		Category:         "duplicate_index",
		Severity:         "critical",
		ObjectType:       "index",
		ObjectIdentifier: subIdent,
		Title: fmt.Sprintf(
			"Subset index %s (covered by %s)", subIdent, supIdent,
		),
		Detail: map[string]any{
			"subset_index": subIdent,
			"superset":     supIdent,
			"subset_def":   sub.IndexDef,
			"superset_def": sup.IndexDef,
		},
		Recommendation: "Drop subset index; the larger index covers it.",
		RecommendedSQL: fmt.Sprintf(
			"DROP INDEX CONCURRENTLY %s;", subIdent,
		),
		RollbackSQL: sub.IndexDef + ";",
		ActionRisk:  "safe",
	}
}

// ruleMissingFKIndexes flags foreign key columns without a supporting index.
func ruleMissingFKIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	// Build set of indexed leading columns per table.
	type tableKey struct{ schema, table string }
	unlogged := buildUnloggedSet(current)
	indexed := make(map[tableKey][][]string)

	for _, idx := range current.Indexes {
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
			"CREATE INDEX CONCURRENTLY ON %s.%s (%s);",
			schema, fk.TableName, fk.FKColumn,
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
			ActionRisk:     "moderate",
		})
	}
	return findings
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
