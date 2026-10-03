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

// ruleInvalidIndexes flags indexes where IsValid is false. An index is
// invalid for the whole duration of CREATE INDEX CONCURRENTLY / REINDEX
// CONCURRENTLY, so (G2-B09) nothing is reported for a table with a build
// in progress, when the build probe failed, or before the index has been
// seen invalid in two cycles (closing the snapshot-vs-probe race).
func ruleInvalidIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	extras *RuleExtras,
) []Finding {
	if extras == nil || extras.IndexBuildProbeFailed {
		return nil
	}
	if extras.InvalidFirstSeen == nil {
		extras.InvalidFirstSeen = make(map[string]time.Time)
	}
	unlogged := buildUnloggedSet(current)
	stillInvalid := make(map[string]bool)
	var findings []Finding
	for _, idx := range current.Indexes {
		ident := idx.SchemaName + "." + idx.IndexRelName
		if idx.IsValid || isSystemSchema(idx.SchemaName) ||
			extras.IndexBuildTables[idx.SchemaName+"."+idx.RelName] {
			continue
		}
		stillInvalid[ident] = true
		if _, seen := extras.InvalidFirstSeen[ident]; !seen {
			extras.InvalidFirstSeen[ident] = time.Now()
			continue
		}
		findings = append(findings, invalidIndexFinding(idx, ident, unlogged))
	}
	for ident := range extras.InvalidFirstSeen {
		if !stillInvalid[ident] {
			delete(extras.InvalidFirstSeen, ident)
		}
	}
	return findings
}

func invalidIndexFinding(
	idx collector.IndexStats, ident string, unlogged map[string]bool,
) Finding {
	severity := "warning"
	rec := "Drop the invalid index and recreate if needed."
	detail := map[string]any{
		"table":     idx.RelName,
		"index_def": idx.IndexDef,
	}
	if unlogged[idx.SchemaName+"."+idx.RelName] {
		severity = "info"
		detail["unlogged"] = true
		rec += " (unlogged table — indexes lost on crash)"
	}
	return Finding{
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
	}
}

// ruleDuplicateIndexes detects exact-duplicate and subset btree indexes.
type duplicateIndexCandidate struct {
	info   collector.IndexStats
	parsed ParsedIndex
}

// ruleDuplicateIndexes flags duplicate and subset btree indexes.
// Duplicates and subsets only exist within one table (IsDuplicate and
// IsSubset compare schema and table first), so pairs are formed per table:
// comparing every index with every other one in the database pinned the
// sidecar on lifeos (35k indexes, ~600M pairs per analyzer cycle).
func ruleDuplicateIndexes(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	var order []tableKey
	byTable := make(map[tableKey][]duplicateIndexCandidate)
	for _, idx := range current.Indexes {
		if isSystemSchema(idx.SchemaName) || !idx.IsValid {
			continue
		}
		p := ParseIndexDef(idx.IndexDef)
		if p.IndexType != "btree" {
			continue
		}
		key := tableKey{p.Schema, p.Table}
		if _, ok := byTable[key]; !ok {
			order = append(order, key)
		}
		byTable[key] = append(byTable[key], duplicateIndexCandidate{info: idx, parsed: p})
	}
	seen := make(map[string]bool)
	var findings []Finding
	for _, key := range order {
		btrees := byTable[key]
		for i := 0; i < len(btrees); i++ {
			for j := i + 1; j < len(btrees); j++ {
				findings = appendPairFinding(findings, seen, btrees[i], btrees[j])
			}
		}
	}
	return findings
}

// appendPairFinding adds the duplicate or subset finding for one pair of
// btree indexes, at most once per index to drop.
func appendPairFinding(
	out []Finding, seen map[string]bool, a, b duplicateIndexCandidate,
) []Finding {
	aIdent := a.info.SchemaName + "." + a.info.IndexRelName
	bIdent := b.info.SchemaName + "." + b.info.IndexRelName
	switch {
	case IsDuplicate(a.parsed, b.parsed):
		drop, keep, dropIdent, keepIdent, ok := chooseDuplicateDrop(a, b, aIdent, bIdent)
		if !ok || seen[dropIdent] {
			return out
		}
		seen[dropIdent] = true
		return append(out, duplicateFinding(drop, keep, dropIdent, keepIdent))
	case IsSubset(a.parsed, b.parsed):
		return appendSubset(out, seen, a, b, aIdent, bIdent)
	case IsSubset(b.parsed, a.parsed):
		return appendSubset(out, seen, b, a, bIdent, aIdent)
	}
	return out
}

// appendSubset adds the finding for sub, an index whose columns lead sup's.
func appendSubset(out []Finding, seen map[string]bool, sub, sup duplicateIndexCandidate,
	subIdent, supIdent string) []Finding {
	if isConstraintBacked(sub.info) ||
		!subsetWorthDropping(sub.parsed, sup.parsed, sub.info, sup.info) || seen[subIdent] {
		return out
	}
	seen[subIdent] = true
	return append(out, subsetFinding(sub.info, sup.info, subIdent, supIdent))
}

func duplicateFinding(drop, keep duplicateIndexCandidate, dropIdent, keepIdent string) Finding {
	return Finding{
		Category:         "duplicate_index",
		Severity:         "critical",
		ObjectType:       "index",
		ObjectIdentifier: dropIdent,
		Title:            fmt.Sprintf("Duplicate index %s (same as %s)", dropIdent, keepIdent),
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
	}
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
