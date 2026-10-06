package fleetlearn

import (
	"testing"
	"time"
)

func rows(n int, category, object, severity string) []FindingRow {
	out := make([]FindingRow, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, FindingRow{Database: string(rune('a' + i)), ID: int64(i + 1),
			Category: category, ObjectIdentifier: object, Severity: severity,
			Title: "Missing index on " + object, LastSeen: time.Unix(int64(1000+i), 0)})
	}
	return out
}

func TestGroupFleetFindings_RecurringOnlyWithDrillDown(t *testing.T) {
	in := append(rows(4, "missing_index", "public.orders|btree(customer_id)", "warning"),
		rows(2, "bloat", "public.events", "info")...)
	got := GroupFleetFindings(in, 3)
	if len(got) != 1 {
		t.Fatalf("fleet findings = %+v, want only the 4-database one", got)
	}
	f := got[0]
	if f.Databases != 4 || len(f.Occurrences) != 4 {
		t.Fatalf("databases=%d occurrences=%d, want 4/4", f.Databases, len(f.Occurrences))
	}
	if f.Category != "missing_index" || f.Severity != "warning" {
		t.Fatalf("group = %+v", f)
	}
	if f.Key == "" || f.Title == "" {
		t.Fatalf("group needs a key and a title: %+v", f)
	}
	for i := 1; i < len(f.Occurrences); i++ {
		if f.Occurrences[i-1].Database > f.Occurrences[i].Database {
			t.Fatal("drill-down not ordered by database")
		}
	}
	if !f.LastSeen.Equal(time.Unix(1003, 0)) {
		t.Fatalf("last seen = %v, want the newest occurrence", f.LastSeen)
	}
}

func TestGroupFleetFindings_SeverityIsWorstAndOrdering(t *testing.T) {
	in := rows(3, "missing_index", "public.a|btree(x)", "info")
	in[1].Severity = "critical"
	in = append(in, rows(5, "unused_index", "public.b_idx", "warning")...)
	got := GroupFleetFindings(in, 2)
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	if got[0].Severity != "critical" {
		t.Fatalf("critical group must sort first, got %+v", got[0])
	}
	if got[1].Databases != 5 {
		t.Fatalf("second group = %+v", got[1])
	}
}

func TestGroupFleetFindings_SameDatabaseTwiceCountsOnce(t *testing.T) {
	in := rows(2, "missing_index", "public.a|btree(x)", "warning")
	dup := in[0]
	dup.ID = 99
	in = append(in, dup)
	got := GroupFleetFindings(in, 3)
	if len(got) != 0 {
		t.Fatalf("2 databases must not reach 3 by duplicate rows: %+v", got)
	}
	got = GroupFleetFindings(in, 2)
	if len(got) != 1 || got[0].Databases != 2 || len(got[0].Occurrences) != 3 {
		t.Fatalf("got %+v, want 2 databases with 3 occurrences", got)
	}
}

func TestGroupFleetFindings_KeyIsCaseAndSpaceInsensitive(t *testing.T) {
	in := []FindingRow{
		{Database: "a", Category: "missing_index", ObjectIdentifier: "Public.Orders|btree(x)"},
		{Database: "b", Category: "missing_index", ObjectIdentifier: " public.orders|btree(x) "},
	}
	if got := GroupFleetFindings(in, 2); len(got) != 1 {
		t.Fatalf("case/space variants must group, got %+v", got)
	}
}

func TestGroupFleetFindings_EmptyAndInvalidMin(t *testing.T) {
	if got := GroupFleetFindings(nil, 2); got == nil || len(got) != 0 {
		t.Fatalf("nil rows must give an empty, non-nil list, got %#v", got)
	}
	in := rows(2, "x", "y", "info")
	if got := GroupFleetFindings(in, 0); len(got) != 1 {
		t.Fatalf("min below 2 is treated as 2: got %+v", got)
	}
	in = append(in, FindingRow{Database: "", Category: "x", ObjectIdentifier: "y"})
	if got := GroupFleetFindings(in, 3); len(got) != 0 {
		t.Fatalf("a row without a database must be ignored: %+v", got)
	}
}

func TestNeedWeight(t *testing.T) {
	idle := NeedWeight(Need{})
	if idle != 1 {
		t.Fatalf("idle weight = %v, want 1", idle)
	}
	busy := NeedWeight(Need{OpenFindings: 20, CriticalFindings: 2, ActiveIncidents: 1})
	if busy <= idle {
		t.Fatal("need must raise the weight")
	}
	incident := NeedWeight(Need{ActiveIncidents: 1})
	open := NeedWeight(Need{OpenFindings: 1})
	if incident <= open {
		t.Fatal("an active incident must outweigh one open finding")
	}
	capped := NeedWeight(Need{OpenFindings: 1e6, CriticalFindings: 1e6, ActiveIncidents: 1e6})
	if capped > MaxNeedWeight {
		t.Fatalf("weight %v exceeds the cap %v", capped, MaxNeedWeight)
	}
	negative := NeedWeight(Need{OpenFindings: -5, CriticalFindings: -1, ActiveIncidents: -2})
	if negative != 1 {
		t.Fatalf("negative counts must be treated as zero, got %v", negative)
	}
}
