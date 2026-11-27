package advisor

import (
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Boundary: units normalize, so 4GB equals 4096MB; enums and booleans
// fold; a setting pending a restart is not live.
func TestSettingEquals(t *testing.T) {
	cases := []struct {
		want string
		live collector.PGSetting
		eq   bool
	}{
		{"4GB", collector.PGSetting{Setting: "4096", Unit: "MB"}, true},
		{"4096MB", collector.PGSetting{Setting: "4096", Unit: "MB"}, true},
		{"'4GB'", collector.PGSetting{Setting: "4096", Unit: "MB"}, true},
		{"4GB", collector.PGSetting{Setting: "2048", Unit: "MB"}, false},
		{"4GB", collector.PGSetting{Setting: "4097", Unit: "MB"}, false},
		{"5min", collector.PGSetting{Setting: "300", Unit: "s"}, true},
		{"16MB", collector.PGSetting{Setting: "2048", Unit: "8kB"}, true},
		{"0.9", collector.PGSetting{Setting: "0.9"}, true},
		{"0.95", collector.PGSetting{Setting: "0.9"}, false},
		{"ON", collector.PGSetting{Setting: "on"}, true},
		{"true", collector.PGSetting{Setting: "on"}, true},
		{"lz4", collector.PGSetting{Setting: "pglz"}, false},
		{"4GB", collector.PGSetting{Setting: "4096", Unit: "MB", PendingRestart: true}, false},
		{"", collector.PGSetting{Setting: "4096", Unit: "MB"}, false},
		// Units are case-sensitive.
		{"4gb", collector.PGSetting{Setting: "4096", Unit: "MB"}, false},
	}
	for _, c := range cases {
		if got := settingEquals(c.want, c.live); got != c.eq {
			t.Errorf("settingEquals(%q, %+v) = %v, want %v", c.want, c.live, got, c.eq)
		}
	}
}

// findingSetting reads ALTER SYSTEM, the managed ALTER DATABASE and the
// managed-change intent; anything else has no value to compare.
func TestFindingSetting(t *testing.T) {
	intent, err := managedparam.NewIntent("rds", "max_wal_size", "4GB", "r")
	if err != nil {
		t.Fatalf("intent: %v", err)
	}
	cases := []struct {
		f           analyzer.Finding
		name, value string
		ok          bool
	}{
		{analyzer.Finding{RecommendedSQL: "ALTER SYSTEM SET max_wal_size = '4GB';"},
			"max_wal_size", "4GB", true},
		{analyzer.Finding{RecommendedSQL: `ALTER DATABASE "app" SET work_mem = '64MB'`},
			"work_mem", "64MB", true},
		{analyzer.Finding{Detail: map[string]any{managedparam.DetailKey: intent.Detail()}},
			"max_wal_size", "4GB", true},
		{analyzer.Finding{RecommendedSQL: "ALTER SYSTEM RESET max_wal_size"}, "", "", false},
		{analyzer.Finding{RecommendedSQL: "garbage; DROP TABLE x"}, "", "", false},
		{analyzer.Finding{}, "", "", false},
	}
	for i, c := range cases {
		name, value, ok := findingSetting(c.f)
		if name != c.name || value != c.value || ok != c.ok {
			t.Errorf("case %d: = (%q, %q, %v), want (%q, %q, %v)", i, name, value, ok,
				c.name, c.value, c.ok)
		}
	}
	advisory := analyzer.Finding{ObjectIdentifier: "instance:WAL_Level"}
	if g := findingGUC(advisory); g != "wal_level" {
		t.Errorf("findingGUC(advisory) = %q, want wal_level", g)
	}
}

func querySnap(ids ...int64) *collector.Snapshot {
	snap := &collector.Snapshot{ConfigData: &collector.ConfigSnapshot{}}
	for _, id := range ids {
		snap.Queries = append(snap.Queries, collector.QueryStats{QueryID: id})
	}
	return snap
}

func keptIdents(r resolution) []string { return findingIdents(r.kept) }

// State transitions: a rewrite resolves after its query is absent for 3
// distinct cycles; a re-checked snapshot does not count twice; the query
// coming back starts the streak over.
func TestResolve_RewriteAbsentThreeCycles(t *testing.T) {
	a := lifecycleAdvisor(walOnlyConfig(""), nil, &fakeOpen{})
	open := []analyzer.Finding{{Category: "query_rewrite", ObjectIdentifier: "queryid:42"}}
	want := []string{"query_rewrite|queryid:42"}
	absent := querySnap(7)
	for i, snap := range []*collector.Snapshot{absent, absent, querySnap(7), querySnap(42),
		querySnap(7), querySnap(7)} {
		if got := keptIdents(a.resolveOpen(snap, open)); !slices.Equal(got, want) {
			t.Fatalf("cycle %d: kept = %v, want still open", i, got)
		}
	}
	if got := keptIdents(a.resolveOpen(querySnap(7), open)); len(got) != 0 {
		t.Fatalf("third absent cycle: kept = %v, want resolved", got)
	}
}

// Nil/empty and invalid input: an unreadable query list (unavailable,
// reset, empty) keeps the rewrite and does not count; a malformed
// identifier stays open.
func TestResolve_RewriteUnreadableKeeps(t *testing.T) {
	a := lifecycleAdvisor(walOnlyConfig(""), nil, &fakeOpen{})
	open := []analyzer.Finding{
		{Category: "query_rewrite", ObjectIdentifier: "queryid:42"},
		{Category: "query_rewrite", ObjectIdentifier: "queryid:abc"},
	}
	reset := querySnap(7)
	reset.StatsReset = true
	missing := querySnap(7)
	missing.Unavailable = map[string]string{"queries": "pg_stat_statements missing"}
	for _, snap := range []*collector.Snapshot{reset, missing, querySnap(), reset, missing} {
		r := a.resolveOpen(snap, open)
		if len(r.kept) != 2 || r.evaluated["query_rewrite"] {
			t.Fatalf("unreadable: kept %v evaluated %v", keptIdents(r), r.evaluated)
		}
	}
	for i := 0; i < rewriteAbsentCycles; i++ {
		r := a.resolveOpen(querySnap(7), open)
		if !slices.Contains(keptIdents(r), "query_rewrite|queryid:abc") {
			t.Fatalf("malformed identifier resolved: %v", keptIdents(r))
		}
	}
}

func bloatSnap(dead int64) *collector.Snapshot {
	return &collector.Snapshot{ConfigData: &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "T1",
			NLiveTup: 10000 - dead, NDeadTup: dead}}}
}

// Boundary: 10% dead is still bloat; under it for 2 cycles resolves. A
// table the snapshot does not name keeps its finding.
func TestResolve_BloatUnderFloorTwoCycles(t *testing.T) {
	a := lifecycleAdvisor(walOnlyConfig(""), nil, &fakeOpen{})
	open := []analyzer.Finding{
		{Category: "bloat_remediation", ObjectIdentifier: `"public"."t1"`},
		{Category: "bloat_remediation", ObjectIdentifier: "public.gone"},
	}
	steps := []struct {
		dead int64
		kept int
	}{{1000, 2}, {999, 2}, {1000, 2}, {999, 2}, {10, 1}}
	for i, s := range steps {
		if got := a.resolveOpen(bloatSnap(s.dead), open); len(got.kept) != s.kept {
			t.Fatalf("step %d (dead %d): kept %v, want %d", i, s.dead, keptIdents(got), s.kept)
		}
	}
	if got := keptIdents(a.resolveOpen(bloatSnap(10), open)); got[0] !=
		"bloat_remediation|public.gone" {
		t.Fatalf("kept = %v, want only the unnamed table", got)
	}
}

// The gate parses every object kind and canonicalizes table names.
func TestOpenGate(t *testing.T) {
	g := newOpenGate([]analyzer.Finding{
		{Category: "wal_tuning", ObjectIdentifier: "instance:max_wal_size"},
		{Category: "connection_tuning", ObjectIdentifier: "instance",
			RecommendedSQL: "ALTER SYSTEM SET max_connections = 200"},
		{Category: "query_rewrite", ObjectIdentifier: "queryid:-9"},
		{Category: "bloat_remediation", ObjectIdentifier: "Orders"},
	})
	if !g.hasGUC("wal_tuning", "MAX_WAL_SIZE") || g.hasGUC("connection_tuning", "max_wal_size") {
		t.Error("wal GUC gate wrong")
	}
	if !g.hasGUC("connection_tuning", "max_connections") || !g.queryIDs[-9] {
		t.Error("connection or query gate wrong")
	}
	if !g.hasTable(collector.TableStats{SchemaName: "public", RelName: "orders"}) {
		t.Error("table gate does not canonicalize")
	}
	var empty openGate
	if empty.hasGUC("wal_tuning", "x") || empty.covers("bloat_remediation",
		analyzer.Finding{ObjectIdentifier: "t"}) {
		t.Error("zero gate gates something")
	}
}

// failureBackoff doubles from 1h and never exceeds the interval.
func TestFailureBackoff(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		failures int
		interval time.Duration
		want     time.Duration
	}{
		{1, day, time.Hour}, {2, day, 2 * time.Hour}, {5, day, 16 * time.Hour},
		{6, day, day}, {60, day, day}, {1, 30 * time.Minute, 30 * time.Minute},
		{1, 0, 0},
	}
	for _, c := range cases {
		if got := failureBackoff(c.failures, c.interval); got != c.want {
			t.Errorf("failureBackoff(%d, %v) = %v, want %v", c.failures, c.interval, got,
				c.want)
		}
	}
}
