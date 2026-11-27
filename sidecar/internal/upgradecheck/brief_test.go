package upgradecheck

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/pgversion"
)

func pgErr(code string) error {
	return &pgconn.PgError{Code: code, Message: "permission denied"}
}

// No concurrent-access test here: newBrief and the hash are pure functions
// of their arguments; the concurrent store is TestBriefDB_ConcurrentStore.

var briefNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// pg14Report is a PG14 run planning pg_upgrade to 17 with one logical
// slot (U3 block), pgvector installed and everything else passing.
func pg14Report(provider string) Report {
	return Report{Database: "orders", SourceVersionNum: 140012, SourceMajor: 14,
		Target:    Target{Major: 17, Method: PGUpgrade, Provider: provider},
		CheckedAt: briefNow,
		Checks: []Check{
			{ID: "U1", Status: Pass, Rows: []Row{}},
			{ID: "U2", Status: Pass, Rows: []Row{}},
			{ID: "U3", Title: "Logical replication slots and subscriptions", Status: Block,
				Reason: "pg_upgrade from PostgreSQL 14 does not carry logical slots",
				Total:  1, Rows: []Row{{"kind": "slot", "slot": "cdc_orders",
					"plugin": "pgoutput", "database": "orders"}}},
			{ID: "U4", Status: Pass, Rows: []Row{}},
			{ID: "U5", Status: Warn, Total: 1, Rows: []Row{{"kind": "collation",
				"name": "en-US-x-icu", "recorded": "153.14", "actual": "153.120"}}},
			{ID: "U6", Status: Pass, Rows: []Row{}},
			{ID: "U7", Status: Pass, Rows: []Row{}},
		}}
}

func pg14Facts() briefFacts {
	return briefFacts{
		installed: []extension{{"vector", "0.8.0"}},
		available: map[string][]string{"vector": {"0.8.0", "0.8.2"}},
		reindex: []ReindexStep{{Action: ReindexAction, Index: "public.docs_title",
			Table: "public.docs", Collations: []string{"en-US-x-icu"}}},
		reindexTotal: 1,
	}
}

func stepIndex(t *testing.T, b Brief, step string) ChecklistStep {
	t.Helper()
	for _, s := range b.Checklist {
		if s.Step == step {
			return s
		}
	}
	t.Fatalf("checklist %+v has no %s step", b.Checklist, step)
	return ChecklistStep{}
}

func TestNewBrief_HappyPathListsAllThreeInChecklistOrder(t *testing.T) {
	b := newBrief(pg14Report("self-managed"), pg14Facts(),
		BriefOptions{SlotConsumers: map[string]string{"cdc_*": "debezium-orders"}})
	if b.Database != "orders" || b.Source.Major != 14 || b.Target.Major != 17 ||
		b.Method != PGUpgrade || !b.BuiltAt.Equal(briefNow) {
		t.Fatalf("brief header = %+v %+v %s %s", b.Source, b.Target, b.Method, b.BuiltAt)
	}
	if len(b.Blockers.Items) != 1 || b.Blockers.Items[0].Check != "U3" ||
		b.Blockers.Summary == NoBlockersFound {
		t.Fatalf("blockers = %+v, want the U3 slot blocker", b.Blockers)
	}
	ext := b.ExtensionPath
	if ext.State != SectionKnown || len(ext.Items) != 1 || ext.Items[0].Extension != "vector" ||
		strings.Join(ext.Items[0].TargetVersions, ",") != "0.8.0,0.8.2" || ext.Items[0].Blocker {
		t.Fatalf("extension path = %+v", ext)
	}
	slot := b.SlotPlan
	if slot.State != SectionKnown || len(slot.Items) != 1 || slot.Items[0].Name != "cdc_orders" ||
		slot.Items[0].Consumer != "debezium-orders" ||
		!strings.Contains(slot.Items[0].Step, "drop and recreate after") ||
		!strings.Contains(slot.Items[0].Step, "blue/green") {
		t.Fatalf("slot plan = %+v", slot)
	}
	re := b.CollationReindex
	if re.State != SectionKnown || len(re.Items) != 1 || re.Items[0].Action != ReindexAction ||
		re.Items[0].Index != "public.docs_title" {
		t.Fatalf("reindex = %+v", re)
	}
	assertChecklist(t, b)
}

func assertChecklist(t *testing.T, b Brief) {
	t.Helper()
	ord := []string{"resolve_blockers", "extension_path", "reindex_plan", "slot_plan",
		"rehearsal", "cutover", "post_upgrade"}
	if len(b.Checklist) != len(ord) {
		t.Fatalf("checklist = %+v, want %v", b.Checklist, ord)
	}
	for i, want := range ord {
		if b.Checklist[i].Step != want || b.Checklist[i].Order != i+1 {
			t.Fatalf("checklist[%d] = %+v, want %s at order %d", i, b.Checklist[i], want, i+1)
		}
	}
	for step, name := range map[string]string{"extension_path": "vector",
		"reindex_plan": "public.docs_title", "slot_plan": "cdc_orders",
		"resolve_blockers": "U3"} {
		if d := stepIndex(t, b, step).Detail; !strings.Contains(d, name) {
			t.Fatalf("%s detail %q does not name %s", step, d, name)
		}
	}
	if c := stepIndex(t, b, "cutover"); c.Owner != "operator" {
		t.Fatalf("cutover = %+v, want the operator's step", c)
	}
	if b.Hash == "" || len(b.Hash) != 64 {
		t.Fatalf("hash = %q, want a sha256 hex", b.Hash)
	}
}

func TestNewBrief_NoBlockersStillHasAChecklist(t *testing.T) {
	rep := Report{Database: "empty", SourceMajor: 14, SourceVersionNum: 140012,
		Target:    Target{Major: 18, Method: PGUpgrade, Provider: "self-managed"},
		CheckedAt: briefNow}
	for _, id := range []string{"U1", "U2", "U3", "U4", "U5", "U6", "U7"} {
		rep.Checks = append(rep.Checks, Check{ID: id, Status: Pass, Rows: []Row{}})
	}
	b := newBrief(rep, briefFacts{}, BriefOptions{})
	if b.Blockers.Summary != NoBlockersFound || len(b.Blockers.Items) != 0 ||
		b.Blockers.Items == nil {
		t.Fatalf("blockers = %+v, want %q with an empty list", b.Blockers, NoBlockersFound)
	}
	if len(b.Checklist) != 7 || b.Checklist[0].Step != "resolve_blockers" ||
		!strings.Contains(b.Checklist[0].Detail, NoBlockersFound) {
		t.Fatalf("checklist = %+v, want all seven steps", b.Checklist)
	}
	if b.ExtensionPath.Items == nil || b.SlotPlan.Items == nil ||
		b.CollationReindex.Items == nil {
		t.Fatalf("empty sections must be [] not null: %+v", b)
	}
	if b.CostOfWaiting.Status != CostNotApplicable {
		t.Fatalf("self-managed cost = %+v, want not_applicable", b.CostOfWaiting)
	}
}

func TestNewBrief_UnknownCheckLeavesItsSectionUnknownWithSQLState(t *testing.T) {
	rep := pg14Report("self-managed")
	rep.Checks[2] = Check{ID: "U3", Status: Unknown, SQLState: "42501", Rows: []Row{},
		Reason: "pg_sage could not read this check's catalog data: permission denied"}
	f := pg14Facts()
	f.reindex, f.reindexTotal = nil, 0
	f.reindexErr = pgErr("42501")
	b := newBrief(rep, f, BriefOptions{})
	if b.SlotPlan.State != SectionUnknown || b.SlotPlan.SQLState != "42501" ||
		len(b.SlotPlan.Items) != 0 {
		t.Fatalf("slot plan = %+v, want unknown with 42501", b.SlotPlan)
	}
	if b.CollationReindex.State != SectionUnknown || b.CollationReindex.SQLState != "42501" {
		t.Fatalf("reindex = %+v, want unknown with 42501", b.CollationReindex)
	}
	if b.ExtensionPath.State != SectionKnown {
		t.Fatalf("extension path = %+v, want it unaffected", b.ExtensionPath)
	}
	if len(b.Blockers.Items) != 0 || strings.Join(b.Blockers.Unknown, ",") != "U3" {
		t.Fatalf("blockers = %+v, want none found and U3 listed unknown", b.Blockers)
	}
	if d := stepIndex(t, b, "slot_plan").Detail; !strings.Contains(d, "unknown") {
		t.Fatalf("slot step detail %q, want it to say unknown", d)
	}
}

func TestNewBrief_ExtensionPathUnknownWithoutTargetMember(t *testing.T) {
	f := pg14Facts()
	f.available, f.availableWhy = nil, "no target_database was named"
	b := newBrief(pg14Report("self-managed"), f, BriefOptions{})
	e := b.ExtensionPath
	if e.State != SectionUnknown || len(e.Items) != 1 || e.Items[0].Path != "unknown" ||
		e.Items[0].TargetVersions != nil || !strings.Contains(e.Reason, "target_database") {
		t.Fatalf("extension path = %+v, want vector listed as unknown", e)
	}
}

func TestNewBrief_ExtensionNotOfferedIsABlocker(t *testing.T) {
	f := pg14Facts()
	f.installed = append(f.installed, extension{"timescaledb", "2.14.0"})
	b := newBrief(pg14Report("self-managed"), f, BriefOptions{})
	var ts ExtensionStep
	for _, e := range b.ExtensionPath.Items {
		if e.Extension == "timescaledb" {
			ts = e
		}
	}
	if !ts.Blocker || len(ts.TargetVersions) != 0 || !strings.Contains(ts.Path, "not offered") {
		t.Fatalf("timescaledb = %+v, want a blocker the target does not offer", ts)
	}
}

func TestNewBrief_HypoPGOnAzureBlocksInPlace(t *testing.T) {
	f := pg14Facts()
	f.installed = append(f.installed, extension{"hypopg", "1.4.1"})
	f.available["hypopg"] = []string{"1.4.1"}
	b := newBrief(pg14Report("azure"), f, BriefOptions{})
	found := false
	for _, bl := range b.Blockers.Items {
		if bl.Check == "U2" && strings.Contains(bl.Title, "hypopg") {
			found = true
		}
	}
	if !found {
		t.Fatalf("azure blockers = %+v, want hypopg as an in-place blocker", b.Blockers)
	}
	rep := pg14Report("azure")
	rep.Target.Method = BlueGreen
	if b := newBrief(rep, f, BriefOptions{}); hasBlocker(b, "hypopg") {
		t.Fatalf("blue/green on azure = %+v, want hypopg not an in-place blocker", b.Blockers)
	}
	if b := newBrief(pg14Report("rds"), f, BriefOptions{}); hasBlocker(b, "hypopg") {
		t.Fatalf("rds = %+v, want hypopg flagged only on azure", b.Blockers)
	}
}

func hasBlocker(b Brief, word string) bool {
	for _, bl := range b.Blockers.Items {
		if strings.Contains(bl.Title, word) {
			return true
		}
	}
	return false
}

func TestSupportState_EndTodayIsSupportedYesterdayEnded(t *testing.T) {
	w, ok := pgversion.Support(pgversion.Community, 14)
	if !ok {
		t.Fatal("no community window for 14")
	}
	end := w.EndTime()
	today := supportState(pgversion.Community, 14, end.Add(23*time.Hour))
	if today.Status != SupportSupported || today.Ends != w.End {
		t.Fatalf("on the end date = %+v, want supported", today)
	}
	after := supportState(pgversion.Community, 14, end.Add(24*time.Hour))
	if after.Status != SupportEnded {
		t.Fatalf("the day after = %+v, want ended", after)
	}
	if u := supportState("azure", 14, end); u.Status != SupportUnknown || u.Ends != "" {
		t.Fatalf("azure = %+v, want unknown: no dated table", u)
	}
}

func TestCostOfWaiting_PricedOnlyWithAnOperatorRate(t *testing.T) {
	rep := pg14Report("rds") // RDS standard support for 14 ends 2027-02-28
	rep.CheckedAt = time.Date(2027, 6, 15, 0, 0, 0, 0, time.UTC)
	unpriced := newBrief(rep, pg14Facts(), BriefOptions{})
	if unpriced.CostOfWaiting.Status != CostUnpriced || unpriced.CostOfWaiting.PerMonth != 0 {
		t.Fatalf("no rate = %+v, want unpriced", unpriced.CostOfWaiting)
	}
	rate := &ExtendedSupportRate{PerVCPUHour: 0.1, VCPUs: 4, Currency: "USD",
		AsOf: "2026-11-01"}
	c := newBrief(rep, pg14Facts(), BriefOptions{Rate: rate}).CostOfWaiting
	if c.Status != CostPriced || c.ExtendedSupportFrom != "2027-03-01" || c.Months != 3 ||
		c.PerMonth != 292 || c.Accrued != 876 || c.Currency != "USD" ||
		!strings.Contains(c.Formula, "730") {
		t.Fatalf("priced = %+v, want 4 vCPU x 0.10 x 730 h = 292/month, 3 months", c)
	}
	if c.DBAHours.Low <= 0 || c.DBAHours.High <= c.DBAHours.Low || c.DBAHours.Formula == "" {
		t.Fatalf("dba hours = %+v, want a range with its formula", c.DBAHours)
	}
	noCPU := newBrief(rep, pg14Facts(), BriefOptions{Rate: &ExtendedSupportRate{
		PerVCPUHour: 0.1, Currency: "USD"}}).CostOfWaiting
	if noCPU.Status != CostUnpriced || !strings.Contains(noCPU.Reason, "vcpus") {
		t.Fatalf("rate without vcpus = %+v, want unpriced naming vcpus", noCPU)
	}
	rep.CheckedAt = time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC)
	early := newBrief(rep, pg14Facts(), BriefOptions{Rate: rate}).CostOfWaiting
	if early.Status != CostPriced || early.Months != 0 || early.Accrued != 0 ||
		early.PerMonth != 292 {
		t.Fatalf("before extended support = %+v, want 0 months accrued", early)
	}
}

func TestBriefHash_IgnoresBuildTimeAndFollowsContent(t *testing.T) {
	a := newBrief(pg14Report("self-managed"), pg14Facts(), BriefOptions{})
	rep := pg14Report("self-managed")
	rep.CheckedAt = briefNow.Add(time.Hour)
	b := newBrief(rep, pg14Facts(), BriefOptions{})
	if a.Hash != b.Hash {
		t.Fatalf("hash changed with only the build time: %s vs %s", a.Hash, b.Hash)
	}
	resolved := pg14Report("self-managed")
	resolved.Checks[2] = Check{ID: "U3", Status: Pass, Rows: []Row{}}
	if c := newBrief(resolved, pg14Facts(), BriefOptions{}); c.Hash == a.Hash {
		t.Fatal("resolving the blocker kept the hash")
	}
}

func TestDefaultTargetMajor(t *testing.T) {
	newest := pgversion.NewestKnownMajor()
	for _, tc := range []struct{ offered, want int }{
		{0, newest}, {-1, newest}, {17, 17}, {newest + 5, newest},
	} {
		if got := DefaultTargetMajor(tc.offered); got != tc.want {
			t.Fatalf("DefaultTargetMajor(%d) = %d, want %d", tc.offered, got, tc.want)
		}
	}
}

func TestTargetValidate_EqualToSourceRefused(t *testing.T) {
	err := Target{Major: 14}.Validate(14)
	if !errors.Is(err, ErrInvalidTarget) || !strings.Contains(err.Error(), "above") {
		t.Fatalf("Validate(14) of 14 = %v, want ErrInvalidTarget", err)
	}
	if err := (Target{Major: 15}).Validate(14); err != nil {
		t.Fatalf("Validate(14) of 15 = %v, want nil", err)
	}
}

func TestBuildBrief_NilPool(t *testing.T) {
	if _, err := BuildBrief(t.Context(), nil, Target{Major: 17}, BriefOptions{}); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil pool = %v, want ErrNoPool", err)
	}
}

func TestStoreBrief_NilPool(t *testing.T) {
	if _, err := StoreBrief(t.Context(), nil, Brief{}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool = %v, want ErrNoPool", err)
	}
}

// An index both on a changed collation and in U5's PG18 case-folding rows
// is one rebuild, listed and counted once.
func TestNewBrief_IndexOnBothReindexListsIsListedOnce(t *testing.T) {
	rep := pg14Report("self-managed")
	rep.Target.Major = 18
	rep.Checks[4].Rows = append(rep.Checks[4].Rows,
		Row{"kind": "index", "name": "public.docs_title", "access_method": "gin"},
		Row{"kind": "index", "name": "public.docs_fts", "access_method": "gin"})
	f := pg14Facts()
	f.reindex[0].Reason = "the collation's recorded version differs"
	re := newBrief(rep, f, BriefOptions{}).CollationReindex
	if re.Total != 2 || len(re.Items) != 2 || re.Items[0].Index != "public.docs_title" ||
		re.Items[1].Index != "public.docs_fts" {
		t.Fatalf("reindex = %+v, want docs_title once then docs_fts (2 in all)", re)
	}
	if !strings.Contains(re.Items[0].Reason, "collation") ||
		!strings.Contains(re.Items[1].Reason, "folds case") {
		t.Fatalf("reasons = %q / %q", re.Items[0].Reason, re.Items[1].Reason)
	}
}
