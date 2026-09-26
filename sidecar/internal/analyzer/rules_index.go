package analyzer

import (
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

type tableKey struct{ schema, table string }

// isSystemSchema reports whether a schema is owned by PostgreSQL or an
// extension and must not be the target of index recommendations. The
// executor protects these from execution, but findings about them should
// never be generated in the first place (they are noise the user can do
// nothing about, e.g. _timescaledb_catalog indexes).
func isSystemSchema(schema string) bool {
	s := strings.ToLower(schema)
	return s == "information_schema" ||
		s == "google_ml" ||
		strings.HasPrefix(s, "pg_") ||
		strings.HasPrefix(s, "_timescaledb")
}

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
			if strings.EqualFold(name, "INDEX") ||
				strings.EqualFold(name, "CONCURRENTLY") ||
				strings.EqualFold(name, "EXISTS") {
				return ""
			}
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
	fkRequirements := buildFKRequirements(current)
	var findings []Finding

	for _, idx := range current.Indexes {
		if isSystemSchema(idx.SchemaName) {
			continue
		}
		if idx.IdxScan > 0 || idx.IsPrimary || idx.IsUnique || !idx.IsValid {
			continue
		}

		// Skip indexes recently created by the executor.
		if _, ok := extras.RecentlyCreated[idx.IndexRelName]; ok {
			continue
		}
		if indexIsOnlyFKSupport(idx, current.Indexes, fkRequirements) {
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

		dropSQL := dropIndexSQL(idx)

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
		if isSystemSchema(idx.SchemaName) {
			continue
		}
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
			RecommendedSQL:   dropIndexSQL(idx),
			RollbackSQL:      idx.IndexDef + ";",
			ActionRisk:       "safe",
		})
	}
	return findings
}

// ruleDuplicateIndexes detects exact-duplicate and subset btree indexes.
type duplicateIndexCandidate struct {
	info   collector.IndexStats
	parsed ParsedIndex
}

func ruleDuplicateIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	var btrees []duplicateIndexCandidate
	for _, idx := range current.Indexes {
		if isSystemSchema(idx.SchemaName) {
			continue
		}
		if !idx.IsValid {
			continue
		}
		p := ParseIndexDef(idx.IndexDef)
		if p.IndexType != "btree" {
			continue
		}
		btrees = append(btrees, duplicateIndexCandidate{
			info: idx, parsed: p,
		})
	}

	seen := make(map[string]bool)
	var findings []Finding

	for i := 0; i < len(btrees); i++ {
		for j := i + 1; j < len(btrees); j++ {
			a, b := btrees[i], btrees[j]
			aIdent := a.info.SchemaName + "." + a.info.IndexRelName
			bIdent := b.info.SchemaName + "." + b.info.IndexRelName

			if IsDuplicate(a.parsed, b.parsed) {
				drop, keep, dropIdent, keepIdent, ok :=
					chooseDuplicateDrop(a, b, aIdent, bIdent)
				if !ok {
					continue
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
					RecommendedSQL: dropIndexSQL(drop.info),
					RollbackSQL:    drop.info.IndexDef + ";",
					ActionRisk:     "safe",
				})
			} else if IsSubset(a.parsed, b.parsed) {
				if isConstraintBacked(a.info) {
					continue
				}
				if !subsetWorthDropping(a.parsed, b.parsed, a.info, b.info) {
					continue
				}
				if seen[aIdent] {
					continue
				}
				seen[aIdent] = true
				findings = append(findings, subsetFinding(
					a.info, b.info, aIdent, bIdent,
				))
			} else if IsSubset(b.parsed, a.parsed) {
				if isConstraintBacked(b.info) {
					continue
				}
				if !subsetWorthDropping(b.parsed, a.parsed, b.info, a.info) {
					continue
				}
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

func chooseDuplicateDrop(
	a, b duplicateIndexCandidate, aIdent, bIdent string,
) (duplicateIndexCandidate, duplicateIndexCandidate, string, string, bool) {
	aProtected := isConstraintBacked(a.info)
	bProtected := isConstraintBacked(b.info)
	switch {
	case aProtected && bProtected:
		return duplicateIndexCandidate{}, duplicateIndexCandidate{},
			"", "", false
	case aProtected:
		return b, a, bIdent, aIdent, true
	case bProtected:
		return a, b, aIdent, bIdent, true
	case a.info.IdxScan > b.info.IdxScan:
		return b, a, bIdent, aIdent, true
	default:
		return a, b, aIdent, bIdent, true
	}
}

func isConstraintBacked(idx collector.IndexStats) bool {
	return idx.IsPrimary || idx.IsUnique
}

const (
	// A wide/large superset is a poor replacement for a narrow index:
	// serving a `WHERE c1 = ?` lookup from a fat composite reads much wider
	// tuples (more I/O per probe) than the dedicated narrow index, so the
	// narrow index earns its keep as a faster access path. Only recommend
	// dropping the subset when the superset is a *close* replacement.
	maxSubsetExtraKeyCols = 2   // superset may add at most this many key cols
	maxSubsetSizeRatio    = 3.0 // superset may be at most this many x the size
	// A heavily-used narrow index is actively serving lookups; even a
	// modestly wider superset would slow them all down, so keep it.
	subsetHeavyUseScans = 100_000
)

// subsetWorthDropping reports whether dropping the narrow `sub` index in
// favor of the wider `sup` is a net win. Column count is the operator's
// intuition ("don't replace (c1) with (c1..c12)"); index byte size is the
// more accurate signal because it accounts for actual column widths, and
// usage tells us whether the narrow index is even earning its keep.
func subsetWorthDropping(
	sub, sup ParsedIndex, subInfo, supInfo collector.IndexStats,
) bool {
	if len(sup.Columns)-len(sub.Columns) > maxSubsetExtraKeyCols {
		return false
	}
	if subInfo.IndexBytes > 0 && supInfo.IndexBytes > 0 &&
		float64(supInfo.IndexBytes) >
			float64(subInfo.IndexBytes)*maxSubsetSizeRatio {
		return false
	}
	// A heavily-used narrow index is worth keeping unless the superset is
	// nearly the same width (≤1 extra key column).
	if subInfo.IdxScan >= subsetHeavyUseScans &&
		len(sup.Columns)-len(sub.Columns) > 1 {
		return false
	}
	return true
}

func subsetFinding(
	sub, sup collector.IndexStats,
	subIdent, supIdent string,
) Finding {
	return Finding{
		Category:         "duplicate_index",
		Severity:         "info",
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
		Recommendation: "Subset index — likely covered by the larger index, " +
			"but a dedicated narrow index can still be faster and may be " +
			"app-managed. Review before dropping.",
		RecommendedSQL: dropIndexSQL(sub),
		RollbackSQL:    sub.IndexDef + ";",
		// Advisory only: a leading-prefix subset drop is a judgment call
		// (read-perf trade-off, and apps that re-create their own indexes
		// turn an auto-drop into an oscillation). high_risk never
		// auto-executes — exact-duplicate drops stay auto (safe).
		ActionRisk: "high_risk",
	}
}

// dropIndexSQL builds the DROP statement with quoted identifiers so a
// mixed-case or unusual name targets exactly this index instead of a
// case-folded different one (G2-B22/G4-B23/C16).
func dropIndexSQL(idx collector.IndexStats) string {
	return "DROP INDEX CONCURRENTLY " +
		sanitize.QuoteQualifiedName(idx.SchemaName, idx.IndexRelName) + ";"
}
