package earned

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// The autonomy ledger (AI-SRE-SPEC §7.3, §10): durable level per family
// x action class, promotion proposed by pg_sage only from evidence and
// taking effect only on a human approval, every change in the history
// with its actor and evidence.

func TestDefaultLevelsAreConservative(t *testing.T) {
	f := newFixture(t)
	if got := f.granted(FamilyLockBlocking, ClassBackendCancel); got != L1 {
		t.Fatalf("shipped family default = %v, want L1", got)
	}
	if got := f.granted("shell", ClassFreeze); got != L0 {
		t.Fatalf("unknown family default = %v, want L0", got)
	}
	st, err := f.svc.Granted(f.ctx, FamilyWAL, ClassWALBound)
	if err != nil || st.Stored || st.Version != 0 || st.ChangedBy != "" {
		t.Fatalf("default state = %+v (%v), want an unstored default", st, err)
	}
}

func TestNoEvidenceNoProposal(t *testing.T) {
	f := newFixture(t)
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil || len(created) != 0 {
		t.Fatalf("proposals without evidence = %+v (%v)", created, err)
	}
}

func TestEvidenceProducesOneStepProposalsUpToTheCap(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[ActionClass]Proposal{}
	for _, p := range created {
		if p.Family != FamilyWAL {
			t.Errorf("proposal for a family without evidence: %+v", p)
		}
		got[p.Class] = p
	}
	bound, ok := got[ClassWALBound]
	if !ok || bound.From != L1 || bound.To != L2 || bound.Status != StatusPending ||
		!bound.ExpiresAt.Equal(fixtureEpoch.Add(7*24*time.Hour)) ||
		len(bound.EvidenceSHA256) != 64 || len(bound.Evidence) == 0 {
		t.Fatalf("wal_bound proposal = %+v", bound)
	}
	if _, ok := got[ClassSlotDrop]; ok {
		t.Fatal("slot_drop is irreversible and must never be proposed above L1")
	}
	again, err := f.svc.ProposePromotions(f.ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second run duplicated proposals: %+v (%v)", again, err)
	}
	evs := f.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound})
	if len(evs) != 1 || evs[0].Type != EventPromotionProposed || evs[0].Actor != ActorPgSage ||
		evs[0].ProposalID != bound.ID || evs[0].To == nil || *evs[0].To != L2 {
		t.Fatalf("history = %+v", evs)
	}
	if f.granted(FamilyWAL, ClassWALBound) != L1 {
		t.Fatal("a proposal alone changed the level")
	}
}

func TestApprovalIsTheTrustStepAndRecordsTheApprover(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWAL, ClassWALBound)
	f.clock.Advance(time.Hour)
	st, err := f.svc.Approve(f.ctx, p.ID, "user:3:dba@example.com", "replay looks right")
	if err != nil {
		t.Fatal(err)
	}
	if st.Level != L2 || st.ChangedBy != "user:3:dba@example.com" || st.Version != 1 ||
		!st.Stored || !strings.Contains(st.Reason, p.ID) || len(st.Evidence) == 0 {
		t.Fatalf("state after approval = %+v", st)
	}
	if f.granted(FamilyWAL, ClassWALBound) != L2 {
		t.Fatal("approval did not take effect")
	}
	got, err := f.svc.Proposal(f.ctx, p.ID)
	if err != nil || got.Status != StatusApproved || got.DecidedBy != "user:3:dba@example.com" ||
		got.DecidedAt == nil || got.Note != "replay looks right" {
		t.Fatalf("decided proposal = %+v (%v)", got, err)
	}
	evs := f.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound})
	if evs[0].Type != EventPromotionApproved || evs[0].Actor != "user:3:dba@example.com" ||
		*evs[0].From != L1 || *evs[0].To != L2 || len(evs[0].Evidence) == 0 {
		t.Fatalf("latest event = %+v", evs[0])
	}
	if _, err := f.svc.Approve(f.ctx, p.ID, "user:3:dba@example.com", ""); !errors.Is(err,
		ErrNotPending) {
		t.Fatalf("second approval: %v", err)
	}
}

func TestOnlyAHumanCanApprove(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWAL, ClassWALBound)
	for _, actor := range []string{"", "  ", "pg_sage", "system", "system-bootstrap",
		"mcp:user:4", "mcp-agent"} {
		if _, err := f.svc.Approve(f.ctx, p.ID, actor, ""); !errors.Is(err,
			ErrHumanApprovalRequired) {
			t.Errorf("approve as %q: %v", actor, err)
		}
	}
	if f.granted(FamilyWAL, ClassWALBound) != L1 {
		t.Fatal("a non-human approval changed the level")
	}
	if _, err := f.svc.Approve(f.ctx, newUUID(t), "user:1:a@b", ""); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown proposal: %v", err)
	}
	if _, err := f.svc.Approve(f.ctx, "not-a-uuid", "user:1:a@b", ""); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("malformed id: %v", err)
	}
}

func TestExpiredProposalCannotBeApproved(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWAL, ClassWALBound)
	f.clock.Advance(7*24*time.Hour + time.Second)
	if _, err := f.svc.Approve(f.ctx, p.ID, "user:1:a@b", ""); !errors.Is(err,
		ErrProposalExpired) {
		t.Fatalf("approve expired: %v", err)
	}
	got, _ := f.svc.Proposal(f.ctx, p.ID)
	if got.Status != StatusExpired || f.granted(FamilyWAL, ClassWALBound) != L1 {
		t.Fatalf("expired proposal = %+v", got)
	}
}

// Approval re-checks the evidence: a proposal whose evidence decayed
// since it was made cannot take effect.
func TestApprovalRechecksEvidence(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWAL, ClassWALBound)
	for i := 0; i < 5; i++ {
		if err := f.svc.RecordReview(f.ctx, Review{Database: f.db,
			InvestigationID: newUUID(t), Family: FamilyWAL, Verdict: VerdictRejected,
			Reviewer: "user:7:ops@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.svc.Approve(f.ctx, p.ID, "user:1:a@b", "")
	var unmet *EvidenceNotMetError
	if !errors.Is(err, ErrEvidenceNotMet) || !errors.As(err, &unmet) ||
		!strings.Contains(err.Error(), "shadow_acceptance") {
		t.Fatalf("approve with decayed evidence: %v", err)
	}
	if f.granted(FamilyWAL, ClassWALBound) != L1 {
		t.Fatal("decayed evidence promoted the pair")
	}
}

func TestRejectKeepsTheLevel(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWAL, ClassWALBound)
	got, err := f.svc.Reject(f.ctx, p.ID, "user:2:ops@example.com", "not yet")
	if err != nil || got.Status != StatusRejected || got.DecidedBy != "user:2:ops@example.com" {
		t.Fatalf("reject = %+v (%v)", got, err)
	}
	if f.granted(FamilyWAL, ClassWALBound) != L1 {
		t.Fatal("reject changed the level")
	}
	if _, err := f.svc.Reject(f.ctx, p.ID, "", "x"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("reject without actor: %v", err)
	}
	evs := f.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound, Limit: 1})
	if len(evs) != 1 || evs[0].Type != EventPromotionRejected {
		t.Fatalf("history = %+v", evs)
	}
}

func TestPromotionIsOneStepAndStopsAtTheCap(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	f.seedL2Recoveries(FamilyWAL, ClassWALBound, 60)
	if st := f.promote(FamilyWAL, ClassWALBound); st.Level != L2 {
		t.Fatalf("level = %v", st.Level)
	}
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range created {
		if p.Class == ClassWALBound {
			t.Fatalf("wal_bound proposed above its L2 cap: %+v", p)
		}
	}
}

func TestManualDowngrade(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	f.promote(FamilyWAL, ClassWALBound)
	sts, err := f.svc.Downgrade(f.ctx, DowngradeRequest{Family: FamilyWAL,
		Class: ClassWALBound, To: L0, Actor: "user:2:ops@example.com",
		Reason: "incident review"})
	if err != nil || len(sts) != 1 || sts[0].Level != L0 ||
		sts[0].ChangedBy != "user:2:ops@example.com" {
		t.Fatalf("downgrade = %+v (%v)", sts, err)
	}
	evs := f.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound, Limit: 1})
	if evs[0].Type != EventDowngraded || *evs[0].From != L2 || *evs[0].To != L0 ||
		evs[0].Reason != "incident review" {
		t.Fatalf("event = %+v", evs[0])
	}
	for name, req := range map[string]DowngradeRequest{
		"same level": {Family: FamilyWAL, Class: ClassWALBound, To: L0, Actor: "u", Reason: "r"},
		"upgrade":    {Family: FamilyWAL, Class: ClassWALBound, To: L2, Actor: "u", Reason: "r"},
	} {
		if _, err := f.svc.Downgrade(f.ctx, req); !errors.Is(err, ErrNotADowngrade) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, req := range map[string]DowngradeRequest{
		"no actor":     {Family: FamilyWAL, Class: ClassWALBound, To: L0, Reason: "r"},
		"no reason":    {Family: FamilyWAL, Class: ClassWALBound, To: L0, Actor: "u"},
		"L4":           {Family: FamilyWAL, Class: ClassWALBound, To: L4, Actor: "u", Reason: "r"},
		"bad family":   {Family: "shell", Class: ClassWALBound, To: L0, Actor: "u", Reason: "r"},
		"bad class":    {Family: FamilyWAL, Class: "rm_rf", To: L0, Actor: "u", Reason: "r"},
		"negative lvl": {Family: FamilyWAL, Class: ClassWALBound, To: -1, Actor: "u", Reason: "r"},
	} {
		if _, err := f.svc.Downgrade(f.ctx, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A family-wide downgrade lowers every class above the target, and a
// downgrade supersedes a pending promotion of that pair.
func TestFamilyWideDowngradeSupersedesProposals(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWraparound, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	freeze := f.pending(FamilyWraparound, ClassFreeze)
	if freeze == nil {
		t.Fatal("no freeze proposal")
	}
	sts, err := f.svc.Downgrade(f.ctx, DowngradeRequest{Family: FamilyWraparound,
		Class: AllClasses, To: L0, Actor: "user:2:o@e", Reason: "failover drill"})
	if err != nil || len(sts) != len(ApplicableClasses(FamilyWraparound)) {
		t.Fatalf("family downgrade = %d states (%v)", len(sts), err)
	}
	for _, c := range ApplicableClasses(FamilyWraparound) {
		if f.granted(FamilyWraparound, c) != L0 {
			t.Errorf("%s not downgraded", c)
		}
	}
	got, _ := f.svc.Proposal(f.ctx, freeze.ID)
	if got.Status != StatusSuperseded {
		t.Fatalf("pending proposal after downgrade = %s", got.Status)
	}
}

// A harmful or unsafe outcome demotes the whole family to at most L1 at
// once (CHECK-40: safety regression), leaves other families alone and
// blocks re-promotion until the evidence window is clean again. The
// ledger is per database (P0-5): the outcome is this database's; another
// database's outcome cannot demote it (scope_db_test.go).
func TestSafetyRegressionDemotesTheFamilyDurably(t *testing.T) {
	f := newFixture(t)
	f.seedL3()
	f.seedL2Evidence(FamilyWAL, 25, 0)
	f.promote(FamilyWAL, ClassWALBound)
	err := f.svc.RecordOutcome(f.ctx, Outcome{Database: f.db, Family: FamilyWraparound,
		Class: ClassVacuum, Level: L1, Result: ResultHarmful, Source: SourceOperator,
		Actor: "user:2:o@e", Detail: "vacuum starved the replica"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.granted(FamilyWraparound, ClassFreeze); got != L1 {
		t.Fatalf("freeze after a family safety regression = %v, want L1", got)
	}
	if got := f.granted(FamilyWAL, ClassWALBound); got != L2 {
		t.Fatalf("another family was demoted: %v", got)
	}
	evs := f.events(EventFilter{Family: FamilyWraparound, Class: ClassFreeze, Limit: 1})
	if evs[0].Type != EventAutoDowngraded || *evs[0].From != L3 || *evs[0].To != L1 ||
		evs[0].Actor != ActorPgSage || !strings.Contains(evs[0].Reason, "harmful") {
		t.Fatalf("event = %+v", evs[0])
	}
	created, err := f.svc.ProposePromotions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range created {
		if p.Family == FamilyWraparound {
			t.Fatalf("re-promotion proposed inside the safety window: %+v", p)
		}
	}
}

func TestRecordOutcomeValidates(t *testing.T) {
	f := newFixture(t)
	good := Outcome{Database: f.db, ActionLogID: 1, Family: FamilyWAL, Class: ClassWALBound,
		Level: L2, Result: ResultVerifiedRecovery, Source: SourceExecutor, Actor: "pg_sage"}
	for name, mutate := range map[string]func(*Outcome){
		"result":   func(o *Outcome) { o.Result = "fine" },
		"source":   func(o *Outcome) { o.Source = "rumour" },
		"family":   func(o *Outcome) { o.Family = "shell" },
		"class":    func(o *Outcome) { o.Class = "Bad Class" },
		"level":    func(o *Outcome) { o.Level = L4 },
		"database": func(o *Outcome) { o.Database = "" },
		"actor":    func(o *Outcome) { o.Actor = "" },
	} {
		o := good
		mutate(&o)
		if err := f.svc.RecordOutcome(f.ctx, o); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.svc.RecordOutcome(f.ctx, good); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RecordOutcome(f.ctx, good); err != nil {
		t.Fatalf("repeat of the same action outcome must be a no-op: %v", err)
	}
	live, err := f.store.LiveStats(f.ctx, FamilyWAL, ClassWALBound)
	if err != nil || live.VerifiedL2 != 1 {
		t.Fatalf("live = %+v (%v), want exactly one recovery", live, err)
	}
}

func TestRecordReviewValidatesAndCountsTheShadowRecord(t *testing.T) {
	f := newFixture(t)
	good := Review{Database: f.db, InvestigationID: newUUID(t), Family: FamilyLockBlocking,
		Verdict: VerdictAccepted, Reviewer: "user:7:o@e"}
	for name, mutate := range map[string]func(*Review){
		"verdict":  func(r *Review) { r.Verdict = "meh" },
		"family":   func(r *Review) { r.Family = "shell" },
		"reviewer": func(r *Review) { r.Reviewer = "" },
		"id":       func(r *Review) { r.InvestigationID = "1; DROP TABLE x" },
		"database": func(r *Review) { r.Database = "" },
		"note":     func(r *Review) { r.Note = strings.Repeat("x", 2001) },
	} {
		r := good
		mutate(&r)
		if err := f.svc.RecordReview(f.ctx, r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := f.svc.RecordReview(f.ctx, good); err != nil {
		t.Fatal(err)
	}
	good.Verdict = VerdictRejected
	if err := f.svc.RecordReview(f.ctx, good); err != nil {
		t.Fatalf("re-review: %v", err)
	}
	sh, err := f.store.ShadowStats(f.ctx, FamilyLockBlocking,
		f.clock.Now().Add(-30*24*time.Hour))
	if err != nil || sh.Reviewed != 1 || sh.Accepted != 0 || sh.FirstReviewAt.IsZero() {
		t.Fatalf("shadow = %+v (%v): a re-review replaces the verdict", sh, err)
	}
}

func TestIngestEvalRunIsIdempotent(t *testing.T) {
	f := newFixture(t)
	raw := benchReport(fixtureEpoch.Add(-time.Hour), FamilyLockBlocking)
	first, err := f.svc.IngestEvalRun(f.ctx, raw, SourceBench, "user:1:a@b", "")
	if err != nil || first.Duplicate || first.ID == "" {
		t.Fatalf("first = %+v (%v)", first, err)
	}
	again, err := f.svc.IngestEvalRun(f.ctx, raw, SourceBench, "user:1:a@b", "")
	if err != nil || !again.Duplicate || again.ID != first.ID {
		t.Fatalf("again = %+v (%v)", again, err)
	}
	if _, err := f.svc.IngestEvalRun(f.ctx, []byte(`{}`), SourceBench, "u", ""); !errors.Is(
		err, ErrInvalidReport) {
		t.Fatalf("invalid report: %v", err)
	}
	if _, err := f.svc.IngestEvalRun(f.ctx, raw, "rumour", "u", ""); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("unknown source: %v", err)
	}
	if _, err := f.svc.IngestEvalRun(f.ctx, raw, SourceGameDay, "pg_sage", ""); !errors.Is(
		err, ErrInvalidRequest) {
		t.Fatalf("a game day without a database: %v", err)
	}
}

// Two admins approving the same proposal at once: exactly one wins.
func TestConcurrentApprovalsPromoteOnce(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	p := f.pending(FamilyWAL, ClassWALBound)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.Approve(f.ctx, p.ID, "user:1:a@b", "")
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrNotPending), errors.Is(err, ErrConflict):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	st, _ := f.svc.Granted(f.ctx, FamilyWAL, ClassWALBound)
	if ok != 1 || st.Level != L2 || st.Version != 1 {
		t.Fatalf("successes = %d, state = %+v", ok, st)
	}
	approved := 0
	for _, e := range f.events(EventFilter{Family: FamilyWAL, Class: ClassWALBound}) {
		if e.Type == EventPromotionApproved {
			approved++
		}
	}
	if approved != 1 {
		t.Fatalf("approval events = %d", approved)
	}
}

func TestViewListsEveryApplicablePairWithItsEvidence(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	if _, err := f.svc.ProposePromotions(f.ctx); err != nil {
		t.Fatal(err)
	}
	v, err := f.svc.View(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Families) != len(Families()) || v.Bench == nil {
		t.Fatalf("view families = %d, bench = %v", len(v.Families), v.Bench)
	}
	var wal *FamilyView
	for i := range v.Families {
		if v.Families[i].Family == FamilyWAL {
			wal = &v.Families[i]
		}
		if len(v.Families[i].Classes) != len(ApplicableClasses(v.Families[i].Family)) {
			t.Errorf("%s rows = %d", v.Families[i].Family, len(v.Families[i].Classes))
		}
	}
	if wal == nil || wal.Shadow.Reviewed < 20 {
		t.Fatalf("wal family view = %+v", wal)
	}
	for _, c := range wal.Classes {
		if c.Class != ClassWALBound {
			continue
		}
		if c.Granted != L1 || c.Cap != L2 || c.Supported < L2 || c.Pending == nil ||
			c.Next == nil || c.Next.Target != L2 || !c.Next.Met ||
			c.Reversibility != MitigationOnly {
			t.Fatalf("wal_bound row = %+v", c)
		}
	}
}

func TestHistoryFiltersAndOrders(t *testing.T) {
	f := newFixture(t)
	f.seedL2Evidence(FamilyWAL, 25, 0)
	f.promote(FamilyWAL, ClassWALBound)
	all := f.events(EventFilter{})
	if len(all) < 2 {
		t.Fatalf("history = %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].At.After(all[i-1].At) || all[i].ID > all[i-1].ID {
			t.Fatalf("history not newest first: %+v", all)
		}
	}
	none := f.events(EventFilter{Family: FamilyLockBlocking})
	if len(none) != 0 {
		t.Fatalf("lock_blocking history = %+v", none)
	}
	if _, err := f.svc.History(f.ctx, EventFilter{Limit: 501}); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("oversized page: %v", err)
	}
}

// Ledgers of different deployments sharing one database never see each
// other's levels.
func TestDeploymentsAreIsolated(t *testing.T) {
	a := newFixture(t)
	b := newFixture(t)
	a.seedL2Evidence(FamilyWAL, 25, 0)
	a.promote(FamilyWAL, ClassWALBound)
	if b.granted(FamilyWAL, ClassWALBound) != L1 {
		t.Fatal("deployment b sees deployment a's level")
	}
	if _, err := NewPostgresStore(a.pool, "not-a-uuid", "orders"); !errors.Is(err,
		ErrInvalidRequest) {
		t.Fatalf("bad deployment id: %v", err)
	}
	if _, err := NewPostgresStore(nil, newUUID(t), "orders"); err == nil {
		t.Fatal("nil pool accepted")
	}
}

func TestEnsureDeploymentIsStable(t *testing.T) {
	pool := testPool(t)
	first, err := EnsureDeployment(t.Context(), pool)
	if err != nil || len(first) != 36 {
		t.Fatalf("deployment = %q (%v)", first, err)
	}
	second, err := EnsureDeployment(t.Context(), pool)
	if err != nil || second != first {
		t.Fatalf("second = %q (%v), want %q", second, err, first)
	}
}
