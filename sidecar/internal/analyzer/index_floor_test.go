package analyzer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// indexes-12: when the tuning agent is inactive (none built, or its model
// unavailable this cycle) the deterministic index floor proposes
// missing_index candidates for seq-scan-heavy tables. Its findings carry
// detail.source = "deterministic_floor"; an unmeasured one is advisory
// (no executable SQL); a shape the agent already proposed is never
// proposed again.

type fakeFloor struct {
	mu     sync.Mutex
	calls  int
	tables []string
	skip   map[string]bool
	res    optimizer.FloorResult
}

func (f *fakeFloor) Run(_ context.Context, _ *collector.Snapshot, tables []string,
	skip map[string]bool) optimizer.FloorResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.tables, f.skip = tables, skip
	return f.res
}

const (
	customerDDL = "CREATE INDEX CONCURRENTLY sage_orders_customer_id_idx ON public.orders " +
		"(customer_id)"
	customerID   = "public.orders|btree(customer_id)"
	regionDDL    = "CREATE INDEX CONCURRENTLY sage_orders_region_idx ON public.orders (region)"
	floorSrcWant = "deterministic_floor"
)

func floorRec(ddl, verdict, reason string) optimizer.Admission {
	return optimizer.Admission{Outcome: optimizer.AdmitAccepted, Rec: optimizer.Recommendation{
		Table: "public.orders", DDL: ddl, Severity: "info", IndexType: "btree",
		Category: optimizer.OptimizerCategory, Rationale: "deterministic floor",
		DropDDL: "DROP INDEX CONCURRENTLY IF EXISTS public.x", WhatIf: verdict,
		WhatIfReason: reason, Validated: verdict == optimizer.WhatIfVerified}}
}

func floorAnalyzer(tp TuningProducer, ff *fakeFloor, qt QueryTuner) (*Analyzer, *[]string) {
	var logs []string
	a := tuningAnalyzer(tp, qt, &logs)
	if ff != nil {
		a.WithIndexFloor(ff)
	}
	a.eval.seqHeavy = []string{"public.orders"}
	return a, &logs
}

func byIdent(fs []Finding, ident string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.ObjectIdentifier == ident {
			out = append(out, f)
		}
	}
	return out
}

// Happy path, no agent: verified keeps its SQL, unmeasured is advisory.
func TestIndexFloor_NoAgentProposesWithTheSource(t *testing.T) {
	ff := &fakeFloor{res: optimizer.FloorResult{Ran: true, Admitted: []optimizer.Admission{
		floorRec(customerDDL, optimizer.WhatIfVerified, ""),
		floorRec(regionDDL, optimizer.WhatIfUnverified, "HypoPG unavailable")}}}
	qt := &deferRecorder{}
	a, _ := floorAnalyzer(nil, ff, qt)
	out := a.runProducers(context.Background(), indexedSnap(), nil)
	if ff.calls != 1 || strings.Join(ff.tables, ",") != "public.orders" {
		t.Fatalf("floor calls %d tables %v", ff.calls, ff.tables)
	}
	if len(out) != 2 {
		t.Fatalf("findings = %+v", out)
	}
	for _, f := range out {
		if f.Category != "missing_index" || f.Detail["source"] != floorSrcWant {
			t.Fatalf("finding %s lacks the floor source: %+v", f.ObjectIdentifier, f.Detail)
		}
	}
	verified := byIdent(out, customerID)
	if len(verified) != 1 || verified[0].RecommendedSQL != customerDDL ||
		verified[0].RollbackSQL == "" {
		t.Fatalf("verified finding = %+v", verified)
	}
	advisory := byIdent(out, "public.orders|btree(region)")
	if len(advisory) != 1 || advisory[0].RecommendedSQL != "" ||
		advisory[0].RollbackSQL != "" ||
		!strings.Contains(advisory[0].Recommendation, "HypoPG not installed: impact unmeasured") {
		t.Fatalf("unmeasured finding must be advisory: %+v", advisory)
	}
	if advisory[0].Detail["ddl"] != regionDDL {
		t.Fatalf("the advisory keeps the DDL for the operator to read: %+v", advisory[0].Detail)
	}
	if !qt.deferred["public.orders"] {
		t.Fatalf("the tuner defers a table the floor proposes for: %v", qt.deferred)
	}
}

// State transition: an active agent (its model answered) runs no floor.
func TestIndexFloor_ActiveAgentRunsNoFloor(t *testing.T) {
	ff := &fakeFloor{res: optimizer.FloorResult{Ran: true,
		Admitted: []optimizer.Admission{floorRec(customerDDL, optimizer.WhatIfVerified, "")}}}
	tp := &fakeTuning{out: TuningOutput{Evaluated: []string{"missing_index"}}}
	a, _ := floorAnalyzer(tp, ff, nil)
	out := a.runProducers(context.Background(), indexedSnap(), nil)
	if ff.calls != 0 || len(out) != 0 {
		t.Fatalf("floor ran beside an active agent: calls %d findings %+v", ff.calls, out)
	}
}

// State transition: the agent's model became unavailable mid-day (budget
// or breaker): the floor runs, and the agent's open shape is skipped, so
// the shape has one finding, not one per path.
func TestIndexFloor_ModelUnavailableRunsTheFloorWithoutDuplicates(t *testing.T) {
	agentF := Finding{Category: "missing_index", ObjectIdentifier: customerID,
		Detail: map[string]any{"producer": "tuning_agent"}, RecommendedSQL: customerDDL}
	tp := &fakeTuning{out: TuningOutput{Findings: []Finding{agentF},
		Evaluated: []string{"missing_index"}, ModelUnavailable: true}}
	// The fake ignores skip: the analyzer must still drop the duplicate.
	ff := &fakeFloor{res: optimizer.FloorResult{Ran: true, Admitted: []optimizer.Admission{
		floorRec(customerDDL, optimizer.WhatIfVerified, ""),
		floorRec(regionDDL, optimizer.WhatIfVerified, "")}}}
	a, _ := floorAnalyzer(tp, ff, nil)
	out := a.runProducers(context.Background(), indexedSnap(), nil)
	if ff.calls != 1 || !ff.skip[customerID] {
		t.Fatalf("floor calls %d, skip %v", ff.calls, ff.skip)
	}
	same := byIdent(out, customerID)
	if len(same) != 1 || same[0].Detail["producer"] != "tuning_agent" {
		t.Fatalf("one finding per shape, the agent's: %+v", same)
	}
	region := byIdent(out, "public.orders|btree(region)")
	if len(region) != 1 || region[0].Detail["source"] != floorSrcWant {
		t.Fatalf("the floor adds the shape the agent did not propose: %+v", out)
	}
}

// Nil/empty: no seq-scan-heavy table, no floor run; no index list, none.
func TestIndexFloor_NoTablesOrNoIndexListRunsNothing(t *testing.T) {
	ff := &fakeFloor{}
	a, _ := floorAnalyzer(nil, ff, nil)
	a.eval.seqHeavy = nil
	a.runProducers(context.Background(), indexedSnap(), nil)
	if ff.calls != 0 {
		t.Fatal("no seq_scan_heavy table still ran the floor")
	}
	a.eval.seqHeavy = []string{"public.orders"}
	a.runProducers(context.Background(),
		&collector.Snapshot{Unavailable: map[string]string{"indexes": "timeout"}}, nil)
	if ff.calls != 0 {
		t.Fatal("without the index list every table looks unindexed")
	}
	var none *Analyzer
	if got := none.runIndexFloor(context.Background(), indexedSnap(), TuningOutput{}); got != nil {
		t.Fatalf("a nil analyzer runs no floor: %v", got)
	}
}

type failingRows struct{ err error }

func (f failingRows) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, f.err
}

// Error propagation: open findings unreadable without an agent: the floor
// cannot tell which shapes are proposed, so it does not run (fail closed).
func TestIndexFloor_OpenFindingsUnreadableSkipsTheFloor(t *testing.T) {
	ff := &fakeFloor{res: optimizer.FloorResult{Ran: true,
		Admitted: []optimizer.Admission{floorRec(customerDDL, optimizer.WhatIfVerified, "")}}}
	a, logs := floorAnalyzer(nil, ff, nil)
	a.floorReader = failingRows{err: errors.New("connection refused")}
	out := a.runProducers(context.Background(), indexedSnap(), nil)
	if ff.calls != 0 || len(out) != 0 {
		t.Fatalf("floor ran blind: calls %d findings %+v", ff.calls, out)
	}
	if !slices.ContainsFunc(*logs, func(l string) bool {
		return strings.HasPrefix(l, "WARN") && strings.Contains(l, "floor")
	}) {
		t.Fatalf("logs = %v", *logs)
	}
}

// Without an agent nothing else re-emits the open index findings: the
// agent's (and older) ones stay open, the floor's own resolve only on the
// tables it fully judged, and a cold start resolves nothing.
func TestIndexFloor_SettleKeepsWhatTheFloorDidNotJudge(t *testing.T) {
	open := map[string]string{
		"public.orders|btree(region)": "",
		"public.items|btree(sku)":     floorSrcWant,
		"public.orders|btree(status)": floorSrcWant,
	}
	a, _ := floorAnalyzer(nil, nil, nil)
	a.settleFloor(open, optimizer.FloorResult{Ran: true, Incomplete: []string{"public.items"}})
	kept := a.eval.kept("missing_index")
	if !kept["public.orders|btree(region)"] || !kept["public.items|btree(sku)"] ||
		kept["public.orders|btree(status)"] {
		t.Fatalf("kept = %v", kept)
	}
	if !a.eval.resolvable(nil)["missing_index"] {
		t.Fatal("a complete floor run lets its own stale findings resolve")
	}
	b, _ := floorAnalyzer(nil, nil, nil)
	b.settleFloor(open, optimizer.FloorResult{})
	kept = b.eval.kept("missing_index")
	if len(kept) != 3 || b.eval.resolvable(nil)["missing_index"] {
		t.Fatalf("a floor that did not run resolves nothing: kept %v", kept)
	}
}

func TestIndexFloor_SeqHeavyTablesOrderedBySeqScans(t *testing.T) {
	snap := &collector.Snapshot{Tables: []collector.TableStats{
		{SchemaName: "public", RelName: "small", SeqScan: 200},
		{SchemaName: "Sales", RelName: "Orders", SeqScan: 900},
		{SchemaName: "public", RelName: "unflagged", SeqScan: 5000}}}
	got := seqHeavyTables(snap, []Finding{
		{Category: "seq_scan_heavy", ObjectIdentifier: "public.small"},
		{Category: "seq_scan_heavy", ObjectIdentifier: "Sales.Orders"},
		{Category: "missing_fk_index", ObjectIdentifier: "public.unflagged"}})
	if strings.Join(got, ",") != `"Sales"."Orders",public.small` {
		t.Fatalf("tables = %v", got)
	}
	if got := seqHeavyTables(snap, nil); got != nil {
		t.Fatalf("no findings, no tables: %v", got)
	}
}
