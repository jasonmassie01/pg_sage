package firstlook

import (
	"strings"
	"testing"
	"time"
)

// No concurrent access tests here: the runway, bloat and schema rules are
// pure functions over values they do not retain.

func TestDefaultThresholdsAreUsable(t *testing.T) {
	th := DefaultThresholds()
	if th.XIDWarnFraction <= 0 || th.XIDCriticalFraction <= th.XIDWarnFraction ||
		th.XIDCriticalFraction >= 1 {
		t.Fatalf("xid thresholds %v/%v", th.XIDWarnFraction, th.XIDCriticalFraction)
	}
	if th.SequenceWarnFraction != 0.70 || th.SequenceCriticalFraction != 0.90 {
		t.Fatalf("sequence thresholds %v/%v, want 0.70/0.90", th.SequenceWarnFraction,
			th.SequenceCriticalFraction)
	}
	if th.BloatMinBytes <= 0 || th.BloatDeadFraction <= 0 || th.FKMinRows < 0 ||
		th.MaxItemsPerRule <= 0 {
		t.Fatalf("thresholds %+v have a zero default", th)
	}
}

func TestXIDRunway(t *testing.T) {
	th := DefaultThresholds()
	const wrap = 2_147_483_647
	healthy := XIDRunway(XIDState{Database: "app", DatFrozenXIDAge: 1000}, th)
	if len(healthy) != 0 {
		t.Fatalf("healthy xid gave %v", rulesOf(healthy))
	}
	warnAge := int64(th.XIDWarnFraction*wrap) + 1
	warn := XIDRunway(XIDState{Database: "app", DatFrozenXIDAge: warnAge,
		OldestTables: []TableAge{{Schema: "public", Table: "events", Age: warnAge}}}, th)
	if len(warn) != 1 || warn[0].Severity != SeverityWarning || warn[0].Rule != RuleXIDRunway {
		t.Fatalf("warn = %+v", warn)
	}
	requireEvidence(t, warn[0])
	if !strings.Contains(warn[0].Detail, "public.events") {
		t.Fatalf("detail %q does not name the oldest table", warn[0].Detail)
	}
	if !strings.Contains(warn[0].Evidence[0].Ref, "datfrozenxid") {
		t.Fatalf("evidence %+v does not cite datfrozenxid", warn[0].Evidence)
	}
	crit := XIDRunway(XIDState{Database: "app",
		DatFrozenXIDAge: int64(th.XIDCriticalFraction*wrap) + 1}, th)
	if len(crit) != 1 || crit[0].Severity != SeverityCritical {
		t.Fatalf("critical = %+v", crit)
	}
}

func TestXIDRunwayBoundary(t *testing.T) {
	th := DefaultThresholds()
	const wrap = 2_147_483_647
	at := int64(th.XIDWarnFraction * wrap)
	if got := XIDRunway(XIDState{Database: "app", DatFrozenXIDAge: at - 1}, th); len(got) != 0 {
		t.Fatalf("below the warning fraction gave %v", rulesOf(got))
	}
	if got := XIDRunway(XIDState{Database: "app", DatFrozenXIDAge: at}, th); len(got) != 1 {
		t.Fatalf("at the warning fraction gave %v", rulesOf(got))
	}
}

func TestMultiXactRunway(t *testing.T) {
	th := DefaultThresholds()
	const wrap = 2_147_483_647
	items := XIDRunway(XIDState{Database: "app",
		DatMinMXIDAge: int64(th.XIDCriticalFraction*wrap) + 5}, th)
	if len(items) != 1 || items[0].Rule != RuleMultiXactRunway ||
		items[0].Severity != SeverityCritical {
		t.Fatalf("multixact = %+v", items)
	}
	if !strings.Contains(items[0].Evidence[0].Ref, "datminmxid") {
		t.Fatalf("evidence %+v does not cite datminmxid", items[0].Evidence)
	}
}

func i64(v int64) *int64 { return &v }

func seq(name string, last *int64, minV, maxV, inc int64, ownerType string) Sequence {
	return Sequence{Schema: "public", Name: name, LastValue: last, Min: minV, Max: maxV,
		Increment: inc, OwnerColumn: "public.t.id", OwnerType: ownerType}
}

func TestSequenceRunway(t *testing.T) {
	th := DefaultThresholds()
	items, unreadable := SequenceRunway([]Sequence{
		seq("healthy", i64(10), 1, 2147483647, 1, "integer"),
		seq("near", i64(2_000_000_000), 1, 2147483647, 1, "integer"),
		seq("unread", nil, 1, 2147483647, 1, "integer"),
	}, th)
	if unreadable != 1 {
		t.Fatalf("unreadable = %d, want 1", unreadable)
	}
	if len(items) != 1 || items[0].Object != "public.near" ||
		items[0].Severity != SeverityCritical || items[0].Rule != RuleSequenceRunway {
		t.Fatalf("items = %+v", items)
	}
	requireEvidence(t, items[0])
	if !strings.Contains(items[0].Detail, "93") {
		t.Fatalf("detail %q does not state the share used (93%%)", items[0].Detail)
	}
}

func TestSequenceRunwayColumnTypeCapsTheLimit(t *testing.T) {
	th := DefaultThresholds()
	// A bigint sequence feeding an integer column runs out at 2^31-1.
	s := seq("mismatch", i64(1_600_000_000), 1, 9223372036854775807, 1, "integer")
	items, _ := SequenceRunway([]Sequence{s}, th)
	if len(items) != 1 || items[0].Severity != SeverityWarning {
		t.Fatalf("items = %+v, want a warning capped by the integer column", items)
	}
	if !strings.Contains(items[0].Detail, "integer") {
		t.Fatalf("detail %q does not name the column type", items[0].Detail)
	}
	if !strings.Contains(items[0].Recommendation, "bigint") {
		t.Fatalf("recommendation %q does not suggest bigint", items[0].Recommendation)
	}
}

func TestSequenceRunwayDescendingAndCycle(t *testing.T) {
	th := DefaultThresholds()
	desc := seq("desc", i64(-2_000_000_000), -2147483648, -1, -1, "integer")
	cyc := seq("cyc", i64(2_100_000_000), 1, 2147483647, 1, "integer")
	cyc.Cycle = true
	items, _ := SequenceRunway([]Sequence{desc, cyc}, th)
	if len(items) != 1 || items[0].Object != "public.desc" {
		t.Fatalf("items = %v, want the descending sequence only (cycle wraps)", rulesOf(items))
	}
}

func TestSequenceRunwayBoundaries(t *testing.T) {
	th := DefaultThresholds()
	span := int64(1000)
	at := func(used int64) []Item {
		items, _ := SequenceRunway([]Sequence{seq("s", i64(used), 1, span, 1, "")}, th)
		return items
	}
	if got := at(699); len(got) != 0 {
		t.Fatalf("69.9%% used gave %v", rulesOf(got))
	}
	if got := at(701); len(got) != 1 || got[0].Severity != SeverityWarning {
		t.Fatalf("70%% used gave %+v", got)
	}
	if got := at(901); len(got) != 1 || got[0].Severity != SeverityCritical {
		t.Fatalf("90%% used gave %+v", got)
	}
	if got, n := SequenceRunway(nil, th); len(got) != 0 || n != 0 {
		t.Fatalf("nil sequences gave %v, %d", got, n)
	}
	zero, _ := SequenceRunway([]Sequence{seq("z", i64(1), 1, 1, 1, "")}, th)
	if len(zero) != 1 || zero[0].Severity != SeverityCritical {
		t.Fatalf("a sequence with no span left gave %+v, want critical", zero)
	}
}

func TestTableBloat(t *testing.T) {
	th := DefaultThresholds()
	big := TableStat{Schema: "public", Table: "events", SizeBytes: th.BloatMinBytes,
		Live: 600, Dead: 400}
	small := TableStat{Schema: "public", Table: "tiny", SizeBytes: th.BloatMinBytes - 1,
		Live: 1, Dead: 99}
	clean := TableStat{Schema: "public", Table: "clean", SizeBytes: th.BloatMinBytes * 10,
		Live: 1000, Dead: 1}
	items := TableBloat([]TableStat{big, small, clean}, th)
	if len(items) != 1 || items[0].Object != "public.events" ||
		items[0].Rule != RuleTableBloat {
		t.Fatalf("items = %v, want events only", rulesOf(items))
	}
	requireEvidence(t, items[0])
	if !strings.Contains(strings.ToLower(items[0].Caveat), "estimate") {
		t.Fatalf("caveat %q does not say this is an estimate", items[0].Caveat)
	}
	if !strings.Contains(items[0].Evidence[0].Ref, "n_dead_tup") {
		t.Fatalf("evidence %+v does not cite n_dead_tup", items[0].Evidence)
	}
	if got := TableBloat([]TableStat{{Schema: "s", Table: "empty",
		SizeBytes: th.BloatMinBytes}}, th); len(got) != 0 {
		t.Fatalf("a table with no tuples gave %v", rulesOf(got))
	}
}

func TestTestSchemas(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	ss := []Schema{{Name: "test_run_1234", Tables: 3, Activity: 0},
		{Name: "tmp_live", Tables: 1, Activity: 50}}
	items := TestSchemas(ss, statsWindow(24*time.Hour, now), now)
	if len(items) != 2 {
		t.Fatalf("items = %v, want both schemas listed", rulesOf(items))
	}
	for _, it := range items {
		requireEvidence(t, it)
		if it.Rule != RuleTestSchema || it.SuggestedSQL != "" {
			t.Fatalf("item %+v: a test schema must never carry SQL to run", it)
		}
	}
	if !strings.Contains(items[0].Detail+items[1].Detail, "no scans or writes") {
		t.Fatalf("details do not report the idle schema: %q / %q", items[0].Detail,
			items[1].Detail)
	}
	if got := TestSchemas(nil, StatsWindow{}, now); len(got) != 0 {
		t.Fatalf("nil schemas gave %v", got)
	}
}

func TestMaxItemsPerRuleCapsAndCounts(t *testing.T) {
	th := DefaultThresholds()
	th.MaxItemsPerRule = 2
	var idx []Index
	for i := uint32(1); i <= 5; i++ {
		x := btree(i, 10+i, "t", 1)
		x.Valid = false
		idx = append(idx, x)
	}
	items, dropped := capItems(InvalidIndexes(idx), th.MaxItemsPerRule)
	if len(items) != 2 || dropped != 3 {
		t.Fatalf("capped = %d dropped = %d, want 2 and 3", len(items), dropped)
	}
	if items, dropped = capItems(nil, 2); len(items) != 0 || dropped != 0 {
		t.Fatalf("nil capped = %v, %d", items, dropped)
	}
}

func TestSortItemsOrdersBySeverityThenObject(t *testing.T) {
	items := []Item{{Rule: "b", Severity: SeverityInfo, Object: "z"},
		{Rule: "a", Severity: SeverityCritical, Object: "y"},
		{Rule: "a", Severity: SeverityWarning, Object: "b"},
		{Rule: "a", Severity: SeverityWarning, Object: "a"}}
	sortItems(items)
	got := []string{items[0].Object, items[1].Object, items[2].Object, items[3].Object}
	want := []string{"y", "a", "b", "z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
