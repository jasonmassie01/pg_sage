package upgradecheck

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// drill-02c: after a major upgrade the producer proposes a plan of typed
// steps: ANALYZE of every table (ordered by the stored brief's query-time
// share, the rest by size), then a reindex of each brief-listed index
// whose collation version now differs. These are the pure parts; the
// producer's reads, its store and its concurrency are in
// postupgrade_db_test.go.

func versionEventOf(id string) changefeed.Event {
	return changefeed.Event{Kind: changefeed.KindVersionChange, EventID: id,
		OccurredAt: time.Date(2026, 11, 2, 3, 4, 0, 0, time.UTC)}
}

func TestPostUpgrade_VersionChangeOfReadsOnlyMajorUpgrades(t *testing.T) {
	got, ok := VersionChangeOf(versionEventOf(
		"version:major:140010->170002@2026-11-02T03:04:00Z"))
	if !ok || got.Old != 140010 || got.New != 170002 || got.NewMajor() != 17 ||
		got.EventID != "version:major:140010->170002@2026-11-02T03:04:00Z" ||
		!got.At.Equal(time.Date(2026, 11, 2, 3, 4, 0, 0, time.UTC)) {
		t.Fatalf("major 14 -> 17 = %+v, %v", got, ok)
	}
	if got, ok := VersionChangeOf(versionEventOf("version:major:90624->100003@x")); !ok ||
		got.Old != 90624 || got.NewMajor() != 10 {
		t.Fatalf("9.6 -> 10 = %+v, %v", got, ok)
	}
	for _, id := range []string{
		"version:minor:170004->170005@2026-11-02T03:04:00Z", // a minor change: nothing
		"version:major:170002->160008@x",                    // a downgrade: nothing
		"version:major:170002->170002@x",                    // no change
		"version:major:abc->170002@x", "version:major:140010->@x", "version:major:",
		"version:major:140010-170002@x", "version:major:14->17@x", "",
	} {
		if got, ok := VersionChangeOf(versionEventOf(id)); ok {
			t.Errorf("VersionChangeOf(%q) = %+v, want nothing", id, got)
		}
	}
	restart := versionEventOf("version:major:140010->170002@x")
	restart.Kind = changefeed.KindRestart
	if _, ok := VersionChangeOf(restart); ok {
		t.Error("a restart event was read as a version change")
	}
}

// sharedBrief is the stored pre-upgrade brief of database app: notes then
// orders by query-time share, and one collation-affected index.
func sharedBrief() *Brief {
	return &Brief{Database: "app", Target: BriefVersion{Major: 17},
		QueryTime: QueryTimeSection{Section: Section{State: SectionKnown}, Total: 2,
			Items: []TableShare{
				{Table: "public.notes", Share: 0.6, QueryIDs: []int64{11, 12}},
				{Table: "public.orders", Share: 0.3, QueryIDs: []int64{13}},
				{Table: "public.gone", Share: 0.1, QueryIDs: []int64{14}},
			}},
		CollationReindex: ReindexSection{Section: Section{State: SectionKnown}, Total: 1,
			Items: []ReindexStep{{Action: ReindexAction, Index: "public.notes_a",
				Table: "public.notes", Collations: []string{"en-US-x-icu"}}}}}
}

func baseInput() postUpgradeInput {
	return postUpgradeInput{Database: "app", DatabaseID: ptrTo(3),
		Change: VersionChange{EventID: "version:major:140010->170002@x", Old: 140010,
			New: 170002},
		Brief: sharedBrief(),
		Tables: []LiveTable{
			{Name: "public.small", Bytes: 1 << 10, CanMaintain: true},
			{Name: "public.orders", Bytes: 1 << 30, CanMaintain: true},
			{Name: "public.logs", Bytes: 5 << 30, CanMaintain: true},
			{Name: "public.notes", Bytes: 10 << 20, CanMaintain: true},
			{Name: "public.mid", Bytes: 1 << 10, CanMaintain: true},
		},
		Reindex: []LiveIndex{{Index: "public.notes_a", Table: "public.notes",
			CanMaintain: true}},
		Fallback: []int64{99},
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestPostUpgradeBuild_MajorChangeFrom14To17IsATwoStepPlan(t *testing.T) {
	d := buildPostUpgrade(baseInput())
	p := d.Plan
	if len(p.Steps) != 2 || len(d.Findings) != 2 {
		t.Fatalf("steps %d findings %d, want 2 and 2: %+v", len(p.Steps), len(d.Findings), p)
	}
	a, r := p.Steps[0], p.Steps[1]
	// Brief order first (notes 0.6, orders 0.3; gone no longer exists), then
	// the rest by size (logs 5 GB), then by name at equal size (mid, small).
	wantSQL := "ANALYZE public.notes, public.orders, public.logs, public.mid, public.small"
	if a.Ordinal != 1 || a.Kind != changeplan.KindAction || a.ActionType != "analyze_table" ||
		a.ProposedSQL != wantSQL || a.RollbackSQL != "" {
		t.Fatalf("step 1 = %+v, want analyze_table %q", a, wantSQL)
	}
	if !slices.Equal(a.TargetQueryIDs, []int64{11, 12, 13}) ||
		a.Metric != changeplan.MetricTimePerCall ||
		a.ExpectedSource != changeplan.SourceNone ||
		a.OnUnverifiable != changeplan.ActionKeep ||
		a.OnInconclusive != changeplan.ActionKeep ||
		a.OnRegressed != changeplan.ActionHaltAsk {
		t.Fatalf("step 1 measure = %+v, want time_per_call on 11,12,13, declared", a)
	}
	if r.Ordinal != 2 || r.ActionType != ReindexAction ||
		r.ProposedSQL != "REINDEX INDEX CONCURRENTLY public.notes_a" ||
		r.RollbackSQL != r.ProposedSQL || !slices.Equal(r.TargetQueryIDs, []int64{11, 12}) ||
		r.OnRegressed != changeplan.ActionHaltAsk {
		t.Fatalf("step 2 = %+v, want the reindex of public.notes_a on notes' queries", r)
	}
	if len(p.Edges) != 1 || p.Edges[0] != (changeplan.Edge{From: 1, To: 2}) {
		t.Fatalf("edges = %v, want 1 -> 2", p.Edges)
	}
	if p.Origin != changeplan.OriginGenerator || p.OriginRef != "post_upgrade:app:170002" ||
		p.Status != changeplan.StatusProposed || p.DatabaseID == nil || *p.DatabaseID != 3 ||
		!strings.Contains(p.Goal, "PostgreSQL 17") || p.CreatedBy == "" {
		t.Fatalf("plan = %+v", p)
	}
	for i, f := range d.Findings {
		if f.SQL != p.Steps[i].ProposedSQL || f.Ident == "" || f.Title == "" {
			t.Fatalf("finding %d = %+v, want step %d's SQL", i, f, i+1)
		}
	}
	if d.Findings[0].Ident == d.Findings[1].Ident {
		t.Fatalf("both findings share ident %q", d.Findings[0].Ident)
	}
	assertValid(t, withFindings(p))
	if len(d.Operator) != 0 || len(d.Skipped) != 0 {
		t.Fatalf("operator %v skipped %v, want none", d.Operator, d.Skipped)
	}
}

// withFindings gives step n finding id n, as the producer does after it
// raises the findings.
func withFindings(p changeplan.Plan) changeplan.Plan {
	for i := range p.Steps {
		p.Steps[i].FindingID = ptrTo(int64(i + 1))
	}
	return p
}

// allResolver resolves every finding and reports no target calls.
type allResolver struct{}

func (allResolver) FindingExists(context.Context, int64) (bool, error) { return true, nil }
func (allResolver) EvidenceExists(context.Context, string, string) (bool, error) {
	return false, nil
}
func (allResolver) TargetCalls(context.Context, []int64, time.Duration) (int64, error) {
	return 0, nil
}
func (allResolver) SettingExists(context.Context, string) (bool, error)      { return true, nil }
func (allResolver) ManagedChangeExists(context.Context, int64) (bool, error) { return true, nil }

func assertValid(t *testing.T, p changeplan.Plan) {
	t.Helper()
	for i := range p.Steps {
		p.Steps[i] = p.Steps[i].WithDefaults()
	}
	problems, err := changeplan.Validate(context.Background(), p, allResolver{})
	if err != nil || len(problems) > 0 {
		t.Fatalf("Validate = %v, %v; want a clean plan", problems, err)
	}
}

func TestPostUpgradeBuild_NoCollationListIsAOneStepPlan(t *testing.T) {
	in := baseInput()
	in.Brief.CollationReindex = ReindexSection{Section: Section{State: SectionKnown}}
	d := buildPostUpgrade(in)
	if len(d.Plan.Steps) != 1 || d.Plan.Steps[0].ActionType != "analyze_table" ||
		len(d.Plan.Edges) != 0 {
		t.Fatalf("plan = %+v, want one ANALYZE step and no edges", d.Plan)
	}
	assertValid(t, withFindings(d.Plan))
}

func TestPostUpgradeBuild_NoBriefOrdersBySizeWithLiveTargets(t *testing.T) {
	in := baseInput()
	in.Brief = nil
	d := buildPostUpgrade(in)
	if len(d.Plan.Steps) != 1 {
		t.Fatalf("plan = %+v, want the ANALYZE step alone (no brief: no collation list)",
			d.Plan)
	}
	a := d.Plan.Steps[0]
	want := "ANALYZE public.logs, public.orders, public.notes, public.mid, public.small"
	if a.ProposedSQL != want || !slices.Equal(a.TargetQueryIDs, []int64{99}) {
		t.Fatalf("step = %q on %v, want %q on the live top queries", a.ProposedSQL,
			a.TargetQueryIDs, want)
	}
	in.Fallback = nil
	d = buildPostUpgrade(in)
	if len(d.Plan.Steps) != 0 || len(d.Operator) != 1 ||
		!strings.Contains(d.Operator[0], "vacuumdb --analyze-in-stages") ||
		!strings.Contains(d.Operator[0], "no target queries") {
		t.Fatalf("no targets: plan %+v operator %v, want no step and vacuumdb text",
			d.Plan, d.Operator)
	}
}

func TestPostUpgradeBuild_EmptyInputsProposeNothing(t *testing.T) {
	d := buildPostUpgrade(postUpgradeInput{Database: "app",
		Change: VersionChange{Old: 140010, New: 170002}})
	if len(d.Plan.Steps) != 0 || len(d.Findings) != 0 {
		t.Fatalf("no tables, no brief = %+v, want no steps", d)
	}
	in := baseInput()
	in.Tables, in.Reindex = nil, nil
	if d := buildPostUpgrade(in); len(d.Plan.Steps) != 0 {
		t.Fatalf("no live tables = %+v, want no steps", d.Plan)
	}
}

func TestPostUpgradeBuild_Target18SkipsAnalyze(t *testing.T) {
	for _, tc := range []struct {
		newVersion int
		analyze    bool
	}{{170009, true}, {180000, false}, {180003, false}, {190001, false}} {
		in := baseInput()
		in.Change.New = tc.newVersion
		d := buildPostUpgrade(in)
		hasAnalyze := len(d.Plan.Steps) > 0 && d.Plan.Steps[0].ActionType == "analyze_table"
		if hasAnalyze != tc.analyze {
			t.Fatalf("target %d: ANALYZE step %v, want %v", tc.newVersion, hasAnalyze,
				tc.analyze)
		}
		if tc.analyze {
			continue
		}
		if len(d.Plan.Steps) != 1 || d.Plan.Steps[0].ActionType != ReindexAction ||
			d.Plan.Steps[0].Ordinal != 1 || len(d.Plan.Edges) != 0 {
			t.Fatalf("target %d: plan %+v, want the reindex alone", tc.newVersion, d.Plan)
		}
		if len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0], StatisticsCarriedOver) {
			t.Fatalf("target %d: skipped %v, want %q", tc.newVersion, d.Skipped,
				StatisticsCarriedOver)
		}
		assertValid(t, withFindings(d.Plan))
	}
}

func TestPostUpgradeBuild_ReindexOnlyBriefListedIndexesThatStillDiffer(t *testing.T) {
	in := baseInput()
	in.Reindex = []LiveIndex{{Index: "public.other", Table: "public.orders",
		CanMaintain: true}}
	d := buildPostUpgrade(in)
	if len(d.Plan.Steps) != 1 || d.Plan.Steps[0].ActionType != "analyze_table" {
		t.Fatalf("plan = %+v, want no reindex: notes_a no longer differs and "+
			"public.other is not in the brief", d.Plan)
	}
}

func TestPostUpgradeBuild_WithoutRightsTheStepsAreOperatorText(t *testing.T) {
	in := baseInput()
	in.Tables[1].CanMaintain = false // public.orders
	in.Reindex[0].CanMaintain = false
	d := buildPostUpgrade(in)
	if len(d.Plan.Steps) != 1 ||
		d.Plan.Steps[0].ProposedSQL !=
			"ANALYZE public.notes, public.logs, public.mid, public.small" {
		t.Fatalf("plan = %+v, want ANALYZE without public.orders and no reindex", d.Plan)
	}
	text := strings.Join(d.Operator, "\n")
	if !strings.Contains(text, "vacuumdb --analyze-in-stages") ||
		!strings.Contains(text, "1 table") ||
		!strings.Contains(text, "REINDEX INDEX CONCURRENTLY public.notes_a") {
		t.Fatalf("operator = %v, want vacuumdb for 1 table and the reindex text", d.Operator)
	}
	for i := range in.Tables {
		in.Tables[i].CanMaintain = false
	}
	if d := buildPostUpgrade(in); len(d.Plan.Steps) != 0 || len(d.Operator) != 2 {
		t.Fatalf("no rights at all: plan %+v operator %v, want operator text only",
			d.Plan, d.Operator)
	}
}

func TestPostUpgradeBuild_StepBoundKeepsFiftyAndNamesTheRest(t *testing.T) {
	in := baseInput()
	in.Brief.CollationReindex.Items = nil
	in.Reindex = nil
	for i := range 60 {
		name := fmt.Sprintf("public.ix_%02d", i)
		in.Brief.CollationReindex.Items = append(in.Brief.CollationReindex.Items,
			ReindexStep{Action: ReindexAction, Index: name, Table: "public.notes"})
		in.Reindex = append(in.Reindex, LiveIndex{Index: name, Table: "public.notes",
			CanMaintain: true})
	}
	d := buildPostUpgrade(in)
	if len(d.Plan.Steps) != 50 || len(d.Plan.Edges) != 49 {
		t.Fatalf("steps %d edges %d, want 50 and 49", len(d.Plan.Steps), len(d.Plan.Edges))
	}
	text := strings.Join(d.Operator, "\n")
	if !strings.Contains(text, "public.ix_49") || strings.Contains(text, "public.ix_48") {
		t.Fatalf("operator = %v, want the 11 indexes past the bound (ix_49..ix_59)",
			d.Operator)
	}
	assertValid(t, withFindings(d.Plan))
}

func TestPostUpgradeBuild_ALongTableListAnalyzesTheWholeDatabase(t *testing.T) {
	in := baseInput()
	in.Tables = nil
	for i := range 800 {
		in.Tables = append(in.Tables, LiveTable{Name: fmt.Sprintf("public.t_%04d", i),
			Bytes: int64(i), CanMaintain: true})
	}
	d := buildPostUpgrade(in)
	if len(d.Plan.Steps) == 0 || d.Plan.Steps[0].ProposedSQL != "ANALYZE" {
		t.Fatalf("800 tables: step 1 = %+v, want a database-wide ANALYZE", d.Plan.Steps)
	}
	assertValid(t, withFindings(d.Plan))
}

func TestPostUpgradeBuild_TargetsAreBoundedAndDeduplicated(t *testing.T) {
	in := baseInput()
	in.Brief.QueryTime.Items = nil
	for i := range 30 {
		in.Brief.QueryTime.Items = append(in.Brief.QueryTime.Items, TableShare{
			Table: "public.notes", Share: 0.01, QueryIDs: []int64{int64(i % 25), 7}})
	}
	d := buildPostUpgrade(in)
	got := d.Plan.Steps[0].TargetQueryIDs
	seen := map[int64]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if len(got) != maxPostUpgradeTargets || len(seen) != len(got) || got[0] != 0 {
		t.Fatalf("targets = %v, want %d distinct", got, maxPostUpgradeTargets)
	}
}

func TestNewPostUpgrade_RefusesMissingDependencies(t *testing.T) {
	if _, err := NewPostUpgrade(PostUpgradeOptions{}); err == nil ||
		!strings.Contains(err.Error(), "pool") {
		t.Fatalf("no deps = %v, want an error naming the pool", err)
	}
}
