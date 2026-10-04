package firstlook

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Index is one user index as the catalog describes it (pg_index, pg_class,
// pg_am, pg_stat_user_indexes).
type Index struct {
	OID, TableOID       uint32
	Schema, Table, Name string
	// Columns is pg_index.indkey: table column numbers, 0 for an expression.
	// The first KeyColumns are key columns, the rest INCLUDE columns.
	Columns      []int16
	KeyColumns   int
	OpClasses    []uint32
	Collations   []uint32
	AccessMethod string
	Unique       bool
	Primary      bool
	// ConstraintBacked: the index enforces a primary key, unique or
	// exclusion constraint and cannot be dropped on its own.
	ConstraintBacked bool
	Valid, Ready     bool
	Predicate        string // pg_get_expr(indpred), "" when none
	Expressions      string // pg_get_expr(indexprs), "" when none
	SizeBytes        int64
	Scans            int64
}

// ForeignKey is one foreign key constraint (pg_constraint contype 'f').
type ForeignKey struct {
	Name, Schema, Table string
	TableOID            uint32
	Columns             []int16
	ColumnNames         []string
	RefTable            string
	// TableRows is pg_class.reltuples of the referencing table; negative
	// when the table was never analyzed.
	TableRows float64
}

const neverScannedWarnAge = 7 * 24 * time.Hour

func qualified(schema, name string) string { return schema + "." + name }

func (x Index) object() string { return qualified(x.Schema, x.Name) }

func (x Index) usable() bool { return x.Valid && x.Ready }

// keys are the key columns (indnkeyatts); a zero KeyColumns means all.
func (x Index) keys() []int16 {
	if x.KeyColumns <= 0 || x.KeyColumns > len(x.Columns) {
		return x.Columns
	}
	return x.Columns[:x.KeyColumns]
}

func (x Index) hasInclude() bool { return len(x.keys()) < len(x.Columns) }

// InvalidIndexes reports indexes a failed concurrent build left invalid:
// they cost writes and serve no reads.
func InvalidIndexes(idx []Index) []Item {
	var out []Item
	for _, x := range idx {
		if x.Valid {
			continue
		}
		name := ident(x.Schema) + "." + ident(x.Name)
		out = append(out, Item{Rule: RuleInvalidIndex, Severity: SeverityWarning,
			Object: x.object(), Title: "Invalid index " + x.object(),
			Detail: fmt.Sprintf("Index %s on %s is invalid (%s): a failed CREATE INDEX "+
				"CONCURRENTLY or REINDEX left it behind. It is maintained on every write "+
				"but never used for reads.", x.object(), qualified(x.Schema, x.Table),
				sizeText(x.SizeBytes)),
			Recommendation: "Rebuild it if it is needed, otherwise drop it.",
			SuggestedSQL:   "REINDEX INDEX CONCURRENTLY " + name + ";",
			Evidence: []Evidence{{Source: "pg_index",
				Ref:    "pg_index.indisvalid = false (indexrelid " + x.object() + ")",
				Detail: fmt.Sprintf("indisready=%t", x.Ready)}}})
	}
	return out
}

// shapeKey identifies what an index stores and in which order.
func shapeKey(x Index) string {
	return fmt.Sprintf("%d|%s|%v|%d|%v|%v|%s|%s", x.TableOID, x.AccessMethod, x.Columns,
		len(x.keys()), x.OpClasses, x.Collations, x.Expressions, x.Predicate)
}

// keepRank orders copies of the same index: the one to keep first.
func keepRank(a, b Index) int {
	switch {
	case a.ConstraintBacked != b.ConstraintBacked:
		return boolRank(a.ConstraintBacked)
	case a.Unique != b.Unique:
		return boolRank(a.Unique)
	case a.OID < b.OID:
		return -1
	case a.OID > b.OID:
		return 1
	}
	return 0
}

func boolRank(first bool) int {
	if first {
		return -1
	}
	return 1
}

// DuplicateIndexes reports exact duplicates (same table, method, columns,
// operator classes, collations, expressions and predicate) and plain btree
// indexes whose columns are a leading prefix of another index. flagged
// holds the OIDs reported, so other rules can skip them.
func DuplicateIndexes(idx []Index) ([]Item, map[uint32]bool) {
	flagged := map[uint32]bool{}
	groups := map[string][]Index{}
	var keys []string
	for _, x := range idx {
		if !x.usable() {
			continue
		}
		k := shapeKey(x)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], x)
	}
	var out []Item
	for _, k := range keys {
		copies := groups[k]
		if len(copies) < 2 {
			continue
		}
		slices.SortFunc(copies, keepRank)
		for _, dup := range copies[1:] {
			if dup.ConstraintBacked {
				continue // dropping it means dropping a constraint
			}
			flagged[dup.OID] = true
			out = append(out, duplicateItem(dup, copies[0]))
		}
	}
	return append(out, redundantPrefixes(idx, flagged)...), flagged
}

func duplicateItem(dup, keep Index) Item {
	return Item{Rule: RuleDuplicateIndex, Severity: SeverityWarning, Object: dup.object(),
		Title: "Duplicate index " + dup.object(),
		Detail: fmt.Sprintf("%s (%s) is identical to %s on %s: same columns, operator "+
			"classes, collations and predicate. Every write maintains both.", dup.object(),
			sizeText(dup.SizeBytes), keep.object(), qualified(dup.Schema, dup.Table)),
		Recommendation: "Drop the duplicate after checking no query hint or constraint " +
			"names it.",
		SuggestedSQL: "DROP INDEX CONCURRENTLY " + ident(dup.Schema) + "." + ident(dup.Name) +
			";",
		Evidence: []Evidence{{Source: "pg_index",
			Ref: "pg_index.indkey, indclass, indcollation (indexrelid " + dup.object() + ")",
			Detail: fmt.Sprintf("indkey=%v indclass=%v same as %s", dup.Columns,
				dup.OpClasses, keep.object())}}}
}

// redundantPrefixes reports plain btree indexes whose columns lead another
// btree index on the same table.
func redundantPrefixes(idx []Index, flagged map[uint32]bool) []Item {
	var out []Item
	for _, a := range idx {
		if !plainBtree(a) || a.Unique || a.ConstraintBacked || flagged[a.OID] {
			continue
		}
		for _, b := range idx {
			if b.OID == a.OID || b.TableOID != a.TableOID || !b.usable() ||
				b.AccessMethod != "btree" || b.Predicate != "" || flagged[b.OID] ||
				!isPrefix(a, b) {
				continue
			}
			flagged[a.OID] = true
			out = append(out, Item{Rule: RuleRedundantIndex, Severity: SeverityInfo,
				Object: a.object(), Title: "Redundant index " + a.object(),
				Detail: fmt.Sprintf("The columns of %s (%s) lead %s, which serves the "+
					"same lookups.", a.object(), sizeText(a.SizeBytes), b.object()),
				Recommendation: "Drop it unless a query relies on its smaller size.",
				SuggestedSQL: "DROP INDEX CONCURRENTLY " + ident(a.Schema) + "." +
					ident(a.Name) + ";",
				Evidence: []Evidence{{Source: "pg_index",
					Ref: "pg_index.indkey (indexrelid " + a.object() + ")",
					Detail: fmt.Sprintf("indkey=%v is a prefix of %s indkey=%v",
						a.Columns, b.object(), b.Columns)}}})
			break
		}
	}
	return out
}

func plainBtree(x Index) bool {
	return x.usable() && x.AccessMethod == "btree" && x.Predicate == "" &&
		x.Expressions == "" && !slices.Contains(x.Columns, 0)
}

// isPrefix reports a's key columns, operator classes and collations
// leading b's key columns. a must have no INCLUDE columns; b's extra key or
// INCLUDE columns serve every lookup a serves. Same-shape copies are exact
// duplicates, reported before this runs.
func isPrefix(a, b Index) bool {
	ak, bk := a.keys(), b.keys()
	n := len(ak)
	if n == 0 || a.hasInclude() || n > len(bk) || shapeKey(a) == shapeKey(b) ||
		len(a.OpClasses) < n || len(b.OpClasses) < n || len(a.Collations) < n ||
		len(b.Collations) < n {
		return false
	}
	return slices.Equal(ak, bk[:n]) && slices.Equal(a.OpClasses[:n], b.OpClasses[:n]) &&
		slices.Equal(a.Collations[:n], b.Collations[:n])
}

// NeverScannedIndexes reports indexes with no scans in the statistics
// window. Constraint indexes, invalid ones and those already reported
// (flagged) are skipped. The caveat states the window: a recent reset, a
// newer index or reads on replicas make zero scans meaningless.
func NeverScannedIndexes(idx []Index, flagged map[uint32]bool, w StatsWindow,
	now time.Time) []Item {
	var out []Item
	for _, x := range idx {
		if x.Scans != 0 || !x.usable() || x.Unique || x.Primary || x.ConstraintBacked ||
			flagged[x.OID] {
			continue
		}
		sev := SeverityInfo
		if w.Known && now.Sub(w.Since) >= neverScannedWarnAge {
			sev = SeverityWarning
		}
		out = append(out, Item{Rule: RuleNeverScannedIndex, Severity: sev,
			Object: x.object(), Title: "Index " + x.object() + " was never scanned",
			Detail: fmt.Sprintf("%s on %s (%s) has idx_scan = 0.", x.object(),
				qualified(x.Schema, x.Table), sizeText(x.SizeBytes)),
			Recommendation: "Confirm no replica or rare job uses it, then consider " +
				"dropping it. pg_sage's unused-index rule keeps watching it.",
			Caveat: windowCaveat(w, now),
			Evidence: []Evidence{{Source: "pg_stat_user_indexes",
				Ref: "pg_stat_user_indexes.idx_scan = 0 (indexrelid " + x.object() + ")"}}})
	}
	return out
}

// windowCaveat states what the zero-scan counters cover.
func windowCaveat(w StatsWindow, now time.Time) string {
	const replicas = " Scans on replicas are not counted here, and an index or " +
		"database created later has a shorter window."
	if !w.Known {
		return "The statistics window is unknown, so zero scans may only mean the " +
			"counters are new." + replicas
	}
	since := "the last statistics reset"
	if w.Source == "server_start" {
		since = "the server started"
	}
	return fmt.Sprintf("Counters cover %s since %s, %s (%s).%s", "this server", since,
		w.Since.UTC().Format("2006-01-02"), ageText(now.Sub(w.Since)), replicas)
}

// UnindexedForeignKeys reports foreign keys whose referencing columns lead
// no usable index: every delete or key update on the referenced table
// scans the referencing table. Tables with fewer than minRows estimated
// rows are skipped; never-analyzed tables are reported.
func UnindexedForeignKeys(fks []ForeignKey, idx []Index, minRows float64) []Item {
	var out []Item
	for _, fk := range fks {
		if fk.TableRows >= 0 && fk.TableRows < minRows {
			continue
		}
		if fkCovered(fk, idx) {
			continue
		}
		table := qualified(fk.Schema, fk.Table)
		cols := strings.Join(fk.ColumnNames, ", ")
		quoted := make([]string, len(fk.ColumnNames))
		for i, c := range fk.ColumnNames {
			quoted[i] = ident(c)
		}
		out = append(out, Item{Rule: RuleUnindexedFK, Severity: SeverityWarning,
			Object: table + "." + fk.Name,
			Title:  fmt.Sprintf("Foreign key %s on %s (%s) has no index", fk.Name, table, cols),
			Detail: fmt.Sprintf("No index on %s leads with (%s). Every delete or key update "+
				"on %s scans %s (%s rows) and holds locks longer.", table, cols, fk.RefTable,
				table, rowsText(fk.TableRows)),
			Recommendation: "Create an index on the referencing columns.",
			SuggestedSQL: fmt.Sprintf("CREATE INDEX CONCURRENTLY ON %s.%s (%s);",
				ident(fk.Schema), ident(fk.Table), strings.Join(quoted, ", ")),
			Evidence: []Evidence{{Source: "pg_constraint",
				Ref: "pg_constraint.conkey (conname " + fk.Name + ", contype f)",
				Detail: fmt.Sprintf("conkey=%v; no pg_index on %s leads with them",
					fk.Columns, table)}}})
	}
	return out
}

func fkCovered(fk ForeignKey, idx []Index) bool {
	n := len(fk.Columns)
	for _, x := range idx {
		if x.TableOID != fk.TableOID || !x.usable() || x.Predicate != "" ||
			len(x.Columns) < n || n == 0 {
			continue
		}
		hashSingle := x.AccessMethod == "hash" && n == 1 && len(x.Columns) == 1
		if x.AccessMethod != "btree" && !hashSingle {
			continue
		}
		if len(x.keys()) < n {
			continue
		}
		lead := slices.Clone(x.keys()[:n])
		want := slices.Clone(fk.Columns)
		slices.Sort(lead)
		slices.Sort(want)
		if slices.Equal(lead, want) {
			return true
		}
	}
	return false
}
