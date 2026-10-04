package firstlook

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests here: the index rules are pure functions over
// slices they do not retain or modify.

func btree(oid uint32, table uint32, name string, cols ...int16) Index {
	ops := make([]uint32, len(cols))
	colls := make([]uint32, len(cols))
	for i := range cols {
		ops[i] = 1978 // int4_ops
	}
	return Index{OID: oid, TableOID: table, Schema: "public", Table: "t", Name: name,
		Columns: cols, OpClasses: ops, Collations: colls, AccessMethod: "btree",
		Valid: true, Ready: true, SizeBytes: 8192 * 10}
}

func rulesOf(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Rule + ":" + it.Object
	}
	return out
}

func requireEvidence(t *testing.T, it Item) {
	t.Helper()
	if len(it.Evidence) == 0 {
		t.Fatalf("item %s %s cites no evidence", it.Rule, it.Object)
	}
	for _, e := range it.Evidence {
		if e.Source == "" || e.Ref == "" {
			t.Fatalf("item %s evidence %+v has no source or ref", it.Rule, e)
		}
	}
}

func TestInvalidIndexesReportsOnlyInvalid(t *testing.T) {
	good := btree(1, 10, "t_ok", 1)
	bad := btree(2, 10, "t_bad", 2)
	bad.Valid = false
	items := InvalidIndexes([]Index{good, bad})
	if len(items) != 1 || items[0].Object != "public.t_bad" ||
		items[0].Rule != RuleInvalidIndex || items[0].Severity != SeverityWarning {
		t.Fatalf("invalid = %+v, want one warning for public.t_bad", items)
	}
	requireEvidence(t, items[0])
	if !strings.Contains(items[0].Evidence[0].Ref, "indisvalid") {
		t.Fatalf("evidence %+v does not cite pg_index.indisvalid", items[0].Evidence)
	}
	if !strings.Contains(items[0].SuggestedSQL, "REINDEX INDEX CONCURRENTLY") &&
		!strings.Contains(items[0].SuggestedSQL, "DROP INDEX CONCURRENTLY") {
		t.Fatalf("suggested SQL %q neither rebuilds nor drops concurrently", items[0].SuggestedSQL)
	}
}

func TestInvalidIndexesNilAndEmpty(t *testing.T) {
	if got := InvalidIndexes(nil); len(got) != 0 {
		t.Fatalf("nil indexes gave %v", got)
	}
	if got := InvalidIndexes([]Index{}); len(got) != 0 {
		t.Fatalf("empty indexes gave %v", got)
	}
}

func TestDuplicateIndexesExactDuplicate(t *testing.T) {
	a := btree(1, 10, "t_a", 1, 2)
	b := btree(2, 10, "t_b", 1, 2)
	items, flagged := DuplicateIndexes([]Index{a, b})
	if len(items) != 1 || items[0].Rule != RuleDuplicateIndex {
		t.Fatalf("items = %v, want one duplicate", rulesOf(items))
	}
	// The later (higher OID) copy is the one to drop; the first is kept.
	if items[0].Object != "public.t_b" || !flagged[2] || flagged[1] {
		t.Fatalf("flagged %v / object %q, want t_b flagged and t_a kept", flagged,
			items[0].Object)
	}
	requireEvidence(t, items[0])
	if !strings.Contains(items[0].Detail, "t_a") {
		t.Fatalf("detail %q does not name the index it duplicates", items[0].Detail)
	}
	if !strings.HasPrefix(items[0].SuggestedSQL, "DROP INDEX CONCURRENTLY") ||
		!strings.Contains(items[0].SuggestedSQL, `"public"."t_b"`) &&
			!strings.Contains(items[0].SuggestedSQL, "public.t_b") {
		t.Fatalf("suggested SQL = %q", items[0].SuggestedSQL)
	}
}

func TestDuplicateIndexesKeepsConstraintBackedCopy(t *testing.T) {
	plain := btree(1, 10, "t_plain", 1)
	pk := btree(2, 10, "t_pkey", 1)
	pk.Unique, pk.Primary, pk.ConstraintBacked = true, true, true
	items, flagged := DuplicateIndexes([]Index{plain, pk})
	if len(items) != 1 || items[0].Object != "public.t_plain" || flagged[2] {
		t.Fatalf("items = %v flagged %v, want the plain copy reported, the pkey kept",
			rulesOf(items), flagged)
	}
}

func TestDuplicateIndexesRedundantPrefix(t *testing.T) {
	short := btree(1, 10, "t_a", 1)
	long := btree(2, 10, "t_ab", 1, 2)
	items, flagged := DuplicateIndexes([]Index{long, short})
	if len(items) != 1 || items[0].Rule != RuleRedundantIndex ||
		items[0].Object != "public.t_a" || !flagged[1] {
		t.Fatalf("items = %v, want t_a redundant to t_ab", rulesOf(items))
	}
	requireEvidence(t, items[0])
}

func TestDuplicateIndexesNotDuplicates(t *testing.T) {
	cases := map[string]func() []Index{
		"different tables": func() []Index {
			return []Index{btree(1, 10, "a", 1), btree(2, 11, "b", 1)}
		},
		"different column order": func() []Index {
			return []Index{btree(1, 10, "a", 1, 2), btree(2, 10, "b", 2, 1)}
		},
		"different access method": func() []Index {
			b := btree(2, 10, "b", 1)
			b.AccessMethod = "hash"
			return []Index{btree(1, 10, "a", 1), b}
		},
		"different operator class": func() []Index {
			b := btree(2, 10, "b", 1)
			b.OpClasses = []uint32{3128}
			return []Index{btree(1, 10, "a", 1), b}
		},
		"different collation": func() []Index {
			b := btree(2, 10, "b", 1)
			b.Collations = []uint32{950}
			return []Index{btree(1, 10, "a", 1), b}
		},
		"partial index": func() []Index {
			b := btree(2, 10, "b", 1)
			b.Predicate = "(a > 0)"
			return []Index{btree(1, 10, "a", 1), b}
		},
		"expression index": func() []Index {
			a, b := btree(1, 10, "a", 0), btree(2, 10, "b", 0)
			a.Expressions, b.Expressions = "lower(x)", "upper(x)"
			return []Index{a, b}
		},
		"unique prefix of non-unique is not redundant": func() []Index {
			u := btree(1, 10, "u", 1)
			u.Unique, u.ConstraintBacked = true, true
			return []Index{u, btree(2, 10, "ab", 1, 2)}
		},
		"invalid copy is reported as invalid, not duplicate": func() []Index {
			b := btree(2, 10, "b", 1)
			b.Valid = false
			return []Index{btree(1, 10, "a", 1), b}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			items, flagged := DuplicateIndexes(mk())
			if len(items) != 0 || len(flagged) != 0 {
				t.Fatalf("items = %v, want none", rulesOf(items))
			}
		})
	}
}

func TestDuplicateIndexesSameExpressionIsDuplicate(t *testing.T) {
	a, b := btree(1, 10, "a", 0), btree(2, 10, "b", 0)
	a.Expressions, b.Expressions = "lower(x)", "lower(x)"
	items, _ := DuplicateIndexes([]Index{a, b})
	if len(items) != 1 || items[0].Rule != RuleDuplicateIndex {
		t.Fatalf("items = %v, want one duplicate", rulesOf(items))
	}
}

func TestDuplicateIndexesThreeCopiesReportTwo(t *testing.T) {
	items, flagged := DuplicateIndexes([]Index{btree(3, 10, "c", 1), btree(1, 10, "a", 1),
		btree(2, 10, "b", 1)})
	if len(items) != 2 || flagged[1] || !flagged[2] || !flagged[3] {
		t.Fatalf("items = %v flagged %v, want b and c reported, a kept", rulesOf(items),
			flagged)
	}
}

func statsWindow(age time.Duration, now time.Time) StatsWindow {
	return StatsWindow{Since: now.Add(-age), Known: true}
}

func TestNeverScannedIndexes(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	unused := btree(1, 10, "t_unused", 1)
	used := btree(2, 10, "t_used", 2)
	used.Scans = 1
	pk := btree(3, 10, "t_pkey", 3)
	pk.Unique, pk.Primary, pk.ConstraintBacked = true, true, true
	dup := btree(4, 10, "t_dup", 4)
	invalid := btree(5, 10, "t_invalid", 5)
	invalid.Valid = false
	items := NeverScannedIndexes([]Index{unused, used, pk, dup, invalid},
		map[uint32]bool{4: true}, statsWindow(30*24*time.Hour, now), now)
	if len(items) != 1 || items[0].Object != "public.t_unused" ||
		items[0].Rule != RuleNeverScannedIndex {
		t.Fatalf("items = %v, want only t_unused", rulesOf(items))
	}
	requireEvidence(t, items[0])
	caveat := items[0].Caveat
	if !strings.Contains(caveat, "2026-09-04") || !strings.Contains(caveat, "30 days") {
		t.Fatalf("caveat %q does not state the statistics window", caveat)
	}
	if !strings.Contains(strings.ToLower(caveat), "replica") {
		t.Fatalf("caveat %q does not warn that replica scans are not counted", caveat)
	}
	if !strings.Contains(items[0].Evidence[0].Ref, "idx_scan") {
		t.Fatalf("evidence %+v does not cite idx_scan", items[0].Evidence)
	}
}

func TestNeverScannedIndexesShortOrUnknownWindowIsInfo(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	idx := []Index{btree(1, 10, "t_unused", 1)}
	short := NeverScannedIndexes(idx, nil, statsWindow(2*time.Hour, now), now)
	if len(short) != 1 || short[0].Severity != SeverityInfo ||
		!strings.Contains(short[0].Caveat, "2 hours") {
		t.Fatalf("short window items = %+v, want info with a 2 hours caveat", short)
	}
	unknown := NeverScannedIndexes(idx, nil, StatsWindow{}, now)
	if len(unknown) != 1 || unknown[0].Severity != SeverityInfo ||
		!strings.Contains(strings.ToLower(unknown[0].Caveat), "unknown") {
		t.Fatalf("unknown window items = %+v, want info with an unknown-window caveat",
			unknown)
	}
	long := NeverScannedIndexes(idx, nil, statsWindow(8*24*time.Hour, now), now)
	if len(long) != 1 || long[0].Severity != SeverityWarning {
		t.Fatalf("8-day window items = %+v, want a warning", long)
	}
}

func TestNeverScannedBoundaryAtSevenDays(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	idx := []Index{btree(1, 10, "t_unused", 1)}
	below := NeverScannedIndexes(idx, nil, statsWindow(7*24*time.Hour-time.Minute, now), now)
	at := NeverScannedIndexes(idx, nil, statsWindow(7*24*time.Hour, now), now)
	if below[0].Severity != SeverityInfo || at[0].Severity != SeverityWarning {
		t.Fatalf("below=%s at=%s, want info below 7 days and warning at 7 days",
			below[0].Severity, at[0].Severity)
	}
}

func fk(table uint32, name string, cols ...int16) ForeignKey {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = string(rune('a' + c - 1))
	}
	return ForeignKey{Name: name, Schema: "public", Table: "child", TableOID: table,
		Columns: cols, ColumnNames: names, RefTable: "public.parent", TableRows: 5000}
}

func TestUnindexedForeignKeys(t *testing.T) {
	covered := fk(10, "fk_covered", 1)
	missing := fk(10, "fk_missing", 2)
	items := UnindexedForeignKeys([]ForeignKey{covered, missing},
		[]Index{btree(1, 10, "child_a", 1, 3)}, 0)
	if len(items) != 1 || items[0].Object != "public.child.fk_missing" ||
		items[0].Rule != RuleUnindexedFK {
		t.Fatalf("items = %v, want fk_missing", rulesOf(items))
	}
	requireEvidence(t, items[0])
	if !strings.HasPrefix(items[0].SuggestedSQL, "CREATE INDEX CONCURRENTLY") ||
		!strings.Contains(items[0].SuggestedSQL, "(b)") {
		t.Fatalf("suggested SQL = %q", items[0].SuggestedSQL)
	}
}

func TestUnindexedForeignKeysCoverage(t *testing.T) {
	multi := fk(10, "fk_ab", 1, 2)
	cases := map[string]struct {
		idx     Index
		covered bool
	}{
		"same order":           {btree(1, 10, "i", 1, 2), true},
		"leading columns swap": {btree(1, 10, "i", 2, 1), true},
		"extra trailing col":   {btree(1, 10, "i", 1, 2, 3), true},
		"only one column":      {btree(1, 10, "i", 1), false},
		"fk cols not leading":  {btree(1, 10, "i", 3, 1, 2), false},
		"other table":          {btree(1, 11, "i", 1, 2), false},
		"partial index": {func() Index {
			i := btree(1, 10, "i", 1, 2)
			i.Predicate = "(a > 0)"
			return i
		}(), false},
		"invalid index": {func() Index {
			i := btree(1, 10, "i", 1, 2)
			i.Valid = false
			return i
		}(), false},
		"hash index cannot serve a two-column key": {func() Index {
			i := btree(1, 10, "i", 1, 2)
			i.AccessMethod = "hash"
			return i
		}(), false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			items := UnindexedForeignKeys([]ForeignKey{multi}, []Index{tc.idx}, 0)
			if (len(items) == 0) != tc.covered {
				t.Fatalf("covered=%v, items=%v", tc.covered, rulesOf(items))
			}
		})
	}
}

func TestUnindexedForeignKeysMinRows(t *testing.T) {
	small := fk(10, "fk_small", 1)
	small.TableRows = 99
	big := fk(11, "fk_big", 1)
	big.TableRows = 100
	items := UnindexedForeignKeys([]ForeignKey{small, big}, nil, 100)
	if len(items) != 1 || !strings.HasSuffix(items[0].Object, "fk_big") {
		t.Fatalf("items = %v, want only the table at the 100-row floor", rulesOf(items))
	}
	if got := UnindexedForeignKeys(nil, nil, 0); len(got) != 0 {
		t.Fatalf("nil fks gave %v", got)
	}
}
