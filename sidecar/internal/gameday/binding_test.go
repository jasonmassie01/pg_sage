package gameday

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// benches-12: the customer program binds to the monitored database's
// hottest permanent leaf table, by the activity delta between its two
// newest collector snapshots (never lifetime counters).
//
// No concurrent access tests: HotTableBinding is a pure function of its
// two snapshot arguments and shares no state.

type tableActivity struct {
	schema, name             string
	ins, upd, del, idx, size int64
	persistence, kind        string
}

func stats(a tableActivity) collector.TableStats {
	if a.persistence == "" {
		a.persistence = "p"
	}
	if a.kind == "" {
		a.kind = "r"
	}
	return collector.TableStats{SchemaName: a.schema, RelName: a.name, NTupIns: a.ins,
		NTupUpd: a.upd, NTupDel: a.del, IdxScan: a.idx, TotalBytes: a.size,
		Relpersistence: a.persistence, Relkind: a.kind}
}

func snap(tables ...tableActivity) *collector.Snapshot {
	s := &collector.Snapshot{Tables: []collector.TableStats{}}
	for _, a := range tables {
		s.Tables = append(s.Tables, stats(a))
	}
	return s
}

func TestHotTableBindingPicksTheLargestActivityDelta(t *testing.T) {
	prev := snap(
		tableActivity{schema: "public", name: "orders", ins: 1000, upd: 500, idx: 9000},
		tableActivity{schema: "public", name: "lifetime_giant", ins: 9_000_000, idx: 1},
		tableActivity{schema: "billing", name: "invoices", ins: 10, upd: 10})
	latest := snap(
		tableActivity{schema: "public", name: "orders", ins: 1100, upd: 550, idx: 9300},
		// The largest lifetime counters, but no activity since the last snapshot.
		tableActivity{schema: "public", name: "lifetime_giant", ins: 9_000_000, idx: 1},
		tableActivity{schema: "billing", name: "invoices", ins: 20, upd: 15, del: 5})
	b := HotTableBinding(latest, prev)
	if !b.Bound() || b.Schema != "public" || b.Table != "orders" || b.Skip != "" ||
		b.Program != ProgramCustomerHotTable {
		t.Fatalf("binding = %+v, want public.orders (delta 450)", b)
	}
}

func TestHotTableBindingExcludesSageCatalogTemporaryAndNonLeafRelations(t *testing.T) {
	prev := snap(
		tableActivity{schema: "sage", name: "snapshots"},
		tableActivity{schema: "pg_catalog", name: "pg_class"},
		tableActivity{schema: "pg_toast", name: "pg_toast_1"},
		tableActivity{schema: "pg_temp_3", name: "scratch"},
		tableActivity{schema: "public", name: "unlogged_q", persistence: "u"},
		tableActivity{schema: "public", name: "temp_q", persistence: "t"},
		tableActivity{schema: "public", name: "events", kind: "p"},
		tableActivity{schema: "public", name: "mv", kind: "m"},
		tableActivity{schema: "public", name: "events_2026", ins: 1})
	latest := snap(
		tableActivity{schema: "sage", name: "snapshots", ins: 1e9},
		tableActivity{schema: "pg_catalog", name: "pg_class", upd: 1e9},
		tableActivity{schema: "pg_toast", name: "pg_toast_1", ins: 1e9},
		tableActivity{schema: "pg_temp_3", name: "scratch", ins: 1e9},
		tableActivity{schema: "public", name: "unlogged_q", ins: 1e9, persistence: "u"},
		tableActivity{schema: "public", name: "temp_q", ins: 1e9, persistence: "t"},
		tableActivity{schema: "public", name: "events", ins: 1e9, kind: "p"},
		tableActivity{schema: "public", name: "mv", idx: 1e9, kind: "m"},
		tableActivity{schema: "public", name: "events_2026", ins: 2})
	b := HotTableBinding(latest, prev)
	if b.Schema != "public" || b.Table != "events_2026" {
		t.Fatalf("binding = %+v, want the permanent leaf public.events_2026", b)
	}
}

// Boundary: equal deltas tie-break by total bytes (larger first), then by
// qualified name, whatever the snapshot order.
func TestHotTableBindingTieBreakIsDeterministic(t *testing.T) {
	cases := []struct {
		name   string
		tables []tableActivity
		want   string
	}{
		{"bytes first", []tableActivity{
			{schema: "public", name: "a_small", size: 10},
			{schema: "public", name: "z_big", size: 20}}, "z_big"},
		{"then name", []tableActivity{
			{schema: "public", name: "zeta", size: 20},
			{schema: "public", name: "alpha", size: 20}}, "alpha"},
		{"schema is part of the name", []tableActivity{
			{schema: "s2", name: "t", size: 20},
			{schema: "s1", name: "t", size: 20}}, "t@s1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, order := range [][]int{{0, 1}, {1, 0}} {
				var prev, latest []tableActivity
				for _, i := range order {
					a := tc.tables[i]
					prev = append(prev, a)
					a.upd = 7
					latest = append(latest, a)
				}
				b := HotTableBinding(snap(latest...), snap(prev...))
				got := b.Table
				if strings.Contains(tc.want, "@") {
					got = b.Table + "@" + b.Schema
				}
				if got != tc.want {
					t.Fatalf("order %v: binding = %+v, want %s", order, b, tc.want)
				}
			}
		})
	}
}

// Nil, empty and boundary inputs give no binding and a reason, never a
// guess.
func TestHotTableBindingSkipsWithAReason(t *testing.T) {
	one := snap(tableActivity{schema: "public", name: "orders", ins: 5})
	sageOnly := snap(tableActivity{schema: "sage", name: "findings", ins: 1})
	sageOnlyLater := snap(tableActivity{schema: "sage", name: "findings", ins: 9})
	quiet := snap(tableActivity{schema: "public", name: "orders", ins: 5})
	reset := snap(tableActivity{schema: "public", name: "orders", ins: 1})
	// A stats reset whose fresh counters already sum past the old ones:
	// the sum grew, but ins went backwards, so there is no delta.
	resetBefore := snap(tableActivity{schema: "public", name: "orders", ins: 100})
	resetGrown := snap(tableActivity{schema: "public", name: "orders", ins: 1, idx: 500})
	unavailable := snap(tableActivity{schema: "public", name: "orders", ins: 9})
	unavailable.Unavailable = map[string]string{"tables": "statement timeout"}
	newOnly := snap(tableActivity{schema: "public", name: "brand_new", ins: 9})
	cases := []struct {
		name           string
		latest, before *collector.Snapshot
		want           string
	}{
		{"no snapshot", nil, nil, SkipNoSnapshot},
		{"one snapshot", one, nil, SkipOneSnapshot},
		{"only sage tables", sageOnlyLater, sageOnly, SkipNoCustomerTable},
		{"empty table lists", snap(), snap(), SkipNoCustomerTable},
		{"no activity", quiet, quiet, SkipNoActivity},
		{"counters went backwards (stats reset)", reset, quiet, SkipNoActivity},
		{"one counter went backwards, the sum grew", resetGrown, resetBefore,
			SkipNoActivity},
		{"table stats unavailable", unavailable, one, SkipNoTableStats},
		{"table absent from the older snapshot", newOnly, one, SkipNoActivity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := HotTableBinding(tc.latest, tc.before)
			if b.Bound() || b.Table != "" || b.Skip != tc.want ||
				b.Program != ProgramCustomerHotTable {
				t.Fatalf("binding = %+v, want skip %q", b, tc.want)
			}
		})
	}
}

// Invalid input: names with quotes, dots and statement separators are
// quoted as one identifier each; the hash never carries the name.
func TestBindingIdentQuotesHostileNames(t *testing.T) {
	cases := []struct {
		schema, table, want string
	}{
		{"public", "orders", `"public"."orders"`},
		{"Sales.EU", `x"; DROP TABLE y; --`, `"Sales.EU"."x""; DROP TABLE y; --"`},
		{"public", "a.b", `"public"."a.b"`},
	}
	for _, tc := range cases {
		b := Binding{Program: ProgramCustomerHotTable, Schema: tc.schema, Table: tc.table}
		if got := b.Ident(); got != tc.want {
			t.Errorf("Ident(%q, %q) = %s, want %s", tc.schema, tc.table, got, tc.want)
		}
		h := b.Hash()
		if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+16 ||
			strings.Contains(h, tc.table) {
			t.Errorf("Hash(%q) = %q, want sha256: and 16 hex digits", tc.table, h)
		}
	}
	a := Binding{Schema: "a.b", Table: "c"}
	b := Binding{Schema: "a", Table: "b.c"}
	if a.Hash() == b.Hash() {
		t.Fatal("a.b|c and a|b.c hash the same: the hash must separate the parts")
	}
	if (Binding{Skip: "x"}).Ident() != "" || (Binding{}).Hash() != "" {
		t.Fatal("an unbound binding has an identifier or a hash")
	}
}

func TestHoldsCustomerObjects(t *testing.T) {
	for provider, want := range map[string]bool{"dle": true, "lakebase_branch": true,
		"local": false, "": false, "mystery": false} {
		if got := HoldsCustomerObjects(provider); got != want {
			t.Errorf("HoldsCustomerObjects(%q) = %v, want %v", provider, got, want)
		}
	}
}
