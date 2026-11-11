package advisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// advisors-05: the bloat prompt lists the tables with the most estimated
// dead bytes, and the trend spans the advisor window.

// No concurrent access tests: the sub-advisors run sequentially in one
// goroutine.

// bloatedCatalog is n qualifying tables in OID order (t00000 first) whose
// estimated dead bytes grow with the index: the oldest are the smallest.
func bloatedCatalog(n int) *collector.Snapshot {
	snap := &collector.Snapshot{CollectedAt: walNow, ConfigData: &collector.ConfigSnapshot{}}
	for i := 0; i < n; i++ {
		snap.Tables = append(snap.Tables, collector.TableStats{SchemaName: "public",
			RelName: fmt.Sprintf("t%05d", i), NLiveTup: 8000, NDeadTup: 2000,
			TableBytes: int64(i+1) << 20})
	}
	return snap
}

func bloatPrompt(t *testing.T, hist windowHistory, snap *collector.Snapshot) string {
	t.Helper()
	mgr, capture := capturingManager(t, "[]")
	ctx := withAdvisorWindow(context.Background(),
		advisorWindow{src: hist, window: 24 * time.Hour})
	if _, err := analyzeBloat(ctx, mgr, snap, config.DefaultConfig(), noopLog); err != nil {
		t.Fatalf("analyzeBloat: %v", err)
	}
	return onlyPrompt(t, capture()).user
}

// Large catalog: 5,000 bloated tables put the 50 with the most dead bytes
// in the prompt with "4,950 omitted" (CHECK-08: within 16 KiB).
func TestBloatRank_LargeCatalogKeepsTheWorst(t *testing.T) {
	user := bloatPrompt(t, &fakeWindowHistory{}, bloatedCatalog(5000))
	for i := 4950; i < 5000; i++ {
		if !strings.Contains(user, fmt.Sprintf("Table: public.t%05d\n", i)) {
			t.Fatalf("prompt lacks the large table t%05d", i)
		}
	}
	if strings.Contains(user, "Table: public.t04949\n") ||
		strings.Contains(user, "Table: public.t00000\n") {
		t.Error("prompt carries a table outside the top 50")
	}
	if n := strings.Count(user, "Table: "); n != 50 {
		t.Errorf("tables listed = %d, want 50", n)
	}
	if !strings.Contains(user, "4,950 omitted") {
		t.Error("prompt does not state the omission count")
	}
	if len(user) > 16<<10 {
		t.Errorf("bloat prompt is %d bytes, want at most 16 KiB", len(user))
	}
	// Ranked worst first.
	if strings.Index(user, "public.t04999") > strings.Index(user, "public.t04950") {
		t.Error("tables are not ranked by dead bytes, worst first")
	}
}

func TestBloatRank_OrderAndTies(t *testing.T) {
	tables := []collector.TableStats{
		{SchemaName: "s", RelName: "b", NLiveTup: 900, NDeadTup: 100, TableBytes: 1000},
		{SchemaName: "s", RelName: "a", NLiveTup: 900, NDeadTup: 100, TableBytes: 1000},
		{SchemaName: "s", RelName: "big", NLiveTup: 5000, NDeadTup: 5000, TableBytes: 150},
		{SchemaName: "s", RelName: "tiny", NLiveTup: 10, NDeadTup: 10, TableBytes: 1 << 40},
		{SchemaName: "s", RelName: "clean", NLiveTup: 10000, NDeadTup: 0, TableBytes: 1 << 40},
	}
	top, omitted := rankBloatCandidates(tables, openGate{}, 2)
	if len(top) != 2 || top[0].RelName != "a" || top[1].RelName != "b" || omitted != 1 {
		t.Fatalf("top = %v omitted %d, want a, b (100 dead bytes each, by name), 1 omitted",
			relNames(top), omitted)
	}
	top, omitted = rankBloatCandidates(nil, openGate{}, 50)
	if len(top) != 0 || omitted != 0 {
		t.Errorf("empty catalog: top %v omitted %d", relNames(top), omitted)
	}
}

func relNames(ts []collector.TableStats) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.RelName)
	}
	return out
}

// The trend compares with the window-start row of the history store; only
// the listed tables are asked for.
func TestBloatTrend_FromTheWindowStart(t *testing.T) {
	snap := &collector.Snapshot{CollectedAt: walNow, ConfigData: &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "grow", NLiveTup: 6000, NDeadTup: 4000,
				TableBytes: 1 << 30},
			{SchemaName: "public", RelName: "shrink", NLiveTup: 8000, NDeadTup: 2000,
				TableBytes: 1 << 29},
			{SchemaName: "public", RelName: "flat", NLiveTup: 8000, NDeadTup: 2000,
				TableBytes: 1 << 28},
			{SchemaName: "public", RelName: "new", NLiveTup: 8000, NDeadTup: 2000,
				TableBytes: 1 << 27},
		}}
	hist := &fakeWindowHistory{tablesFound: true, tables: tablesStart{
		at: walNow.Add(-20 * time.Hour), tables: map[tableKey]deadCount{
			{"public", "grow"}:   {live: 9000, dead: 1000},
			{"public", "shrink"}: {live: 5000, dead: 5000},
			{"public", "flat"}:   {live: 8100, dead: 1900},
		}}}
	user := bloatPrompt(t, hist, snap)
	for table, want := range map[string]string{"grow": "growing", "shrink": "shrinking",
		"flat": "stable", "new": "unknown"} {
		block := tableBlock(user, "public."+table)
		if !strings.Contains(block, "Dead-tuple trend since 2026-11-10 16:00 UTC: "+want) {
			t.Errorf("%s: block %q, want trend %s", table, block, want)
		}
	}
	if len(hist.gotKeys) != 4 || hist.gotWindow != 24*time.Hour {
		t.Errorf("history asked for %v over %s, want the 4 listed tables over 24h",
			hist.gotKeys, hist.gotWindow)
	}
}

func tableBlock(user, name string) string {
	i := strings.Index(user, "Table: "+name+"\n")
	if i < 0 {
		return ""
	}
	block := user[i:]
	if j := strings.Index(block, "\n\n"); j >= 0 {
		block = block[:j]
	}
	return block
}

// No row in the window, a row too recent to be a trend, or a history read
// error: every trend is unknown, never "stable" against a minute-old row.
func TestBloatTrend_UnknownWithoutAWindowRow(t *testing.T) {
	snap := deadTupleSnapshot("t1")
	snap.CollectedAt = walNow
	recent := &fakeWindowHistory{tablesFound: true, tables: tablesStart{
		at: walNow.Add(-time.Minute), tables: map[tableKey]deadCount{
			{"public", "t1"}: {live: 10000, dead: 5000}}}}
	for name, hist := range map[string]windowHistory{"no source": nil,
		"no row": &fakeWindowHistory{}, "a minute old": recent,
		"read error": &fakeWindowHistory{tablesErr: errors.New("timeout")}} {
		user := bloatPrompt(t, hist, snap)
		if !strings.Contains(user, "Dead-tuple trend: unknown") ||
			strings.Contains(user, "stable") {
			t.Errorf("%s: prompt %q, want an unknown trend", name, user)
		}
	}
}

// The bloat advisor stays advisory: no SQL, info severity, even when the
// model proposes VACUUM FULL.
func TestBloatAdvisor_NeverExecutable(t *testing.T) {
	mgr, _ := capturingManager(t, `[{"object_identifier":"public.t1","severity":"critical",`+
		`"rationale":"rewrite","recommended_sql":"VACUUM FULL public.t1"}]`)
	snap := deadTupleSnapshot("t1")
	got, err := analyzeBloat(context.Background(), mgr, snap, config.DefaultConfig(), noopLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RecommendedSQL != "" || got[0].Severity != "info" {
		t.Fatalf("findings = %+v, want one advisory info row", got)
	}
}
