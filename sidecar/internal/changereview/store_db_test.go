package changereview

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/verify"
)

// CHECK-FND-03: the store round-trips every typed field, and every
// transition is a conditional UPDATE: a DBA verdict is recorded once,
// only on a complete review; override needs a reason; ten racing
// deciders store exactly one verdict.

func fullReview(t *testing.T) Review {
	now := utc(time.Now().Add(-2 * time.Hour))
	pct := -35.5
	return Review{Database: "testdb", Source: SourceMCP, Actor: "mcp:token:ci",
		Repo: "acme/shop", PRURL: "https://github.com/acme/shop/pull/7", PRNumber: 7,
		HeadSHA: "abc1234", BaseSHA: "def5678", PRTitle: "Rename email",
		PROpenedAt: ptr(now.Add(-time.Hour)), MigrationTool: "rails",
		Transactional: ptr(true), RevisionOf: "0f14e45f-ceea-467a-9575-0c9a1d8e7a01",
		InputHash: strings.Repeat("a", 64), ContentHash: strings.Repeat("b", 64),
		DiffSHA256: strings.Repeat("c", 64), State: StateComplete, Stage: StageDone,
		Verdict: VerdictRisky, DeterministicVerdict: VerdictRisky, ParseMode: ParseAST,
		ModelStatus: ModelOK,
		Statements: []Statement{{Index: 1, Origin: OriginModelTranslated,
			File: "db/migrate/1.rb", Line: 4, SQL: "ALTER TABLE orders RENAME email TO e",
			Kind: "rename_column", Relations: []string{"public.orders"},
			Fingerprint: "fp1", MatchedQueryID: ptr(int64(-42)), PlanSummary: "Seq Scan",
			Confidence: 0.75, EvidenceID: "stmt:1", ORM: "activerecord", InLoop: true}},
		Findings: []Finding{ruleFinding("CR-03", SeverityCritical), {Kind: "fix",
			Severity: SeverityWarning, Origin: OriginModel, Title: "t", Text: "x",
			EvidenceIDs: []string{"stmt:1"},
			Suggestion: &Suggestion{Language: "ruby", Code: "add_column :o, :e, :text"}}},
		Evidence: []Evidence{mustEvidence(t, "stmt", "1", "statement 1",
			map[string]any{"calls": 2000, "mean_ms": 0.5, "nested": map[string]any{"a": []any{1, "x"}}})},
		Dropped:       []Dropped{{Kind: "claim", Text: "uncited", Reason: "uncited"}},
		NotDetermined: []NotDetermined{{Subject: "app/models/x.rb", Reason: "cut"}},
		Summary: ReviewSummary{Critical: 1, Warning: 1, Rules: []string{"CR-03"},
			ModelFindings: 1, DroppedCount: 1, AppCodeAnalyzed: true, Intent: "contract",
			Calibration: &Calibration{Since: now.Add(-90 * 24 * time.Hour), Reviews: 3,
				FalseSafe: 1, ByVerdict: map[string]VerdictClass{VerdictSafe: {Reviews: 2,
					Decided: 1, Concurred: 1, Receipts: 1, Clean: 0, Regressed: 1}}}},
		Expectations: Expectations{PostConditions: []PostCondition{{StatementIndex: 1,
			Kind: PostColumnRenamed, Relation: "public.orders", Column: "email",
			NewName: "e"}}, WatchQueryIDs: []int64{-42, 7}, Relations: []string{"public.orders"},
			Predictions: []verify.Prediction{{Class: "change_review", Method: "rule",
				TargetQueryIDs: []int64{-42}, ExpectedChangePct: &pct}}},
		Transcript: &agentloop.Transcript{Protocol: agentloop.ProtocolNative, Stop: "final",
			ModelCalls: 2, Tokens: 1234, Steps: []agentloop.Step{}},
		Markdown: "<!-- marker -->", Tokens: 1234, ReceiptHash: strings.Repeat("d", 64),
		DBAVerdict: DBAOverride, DBADirection: DirectionTooStrict, DBAReason: "known",
		DBAActor: "user:1", DBADecidedAt: ptr(now.Add(time.Minute)),
		VerdictCarriedFrom: "1f14e45f-ceea-467a-9575-0c9a1d8e7a01",
		LandingState: LandingLanded, LandingSource: LandingSourceChangeEvent,
		LandedLower: ptr(now.Add(2 * time.Minute)), LandedAt: ptr(now.Add(3 * time.Minute)),
		ReceiptVerdict: ReceiptRegressed, Receipt: &Receipt{Verdict: ReceiptRegressed,
			Reason: "latency", WindowHours: 24, BeforeFrom: now, BeforeTo: now, AfterFrom: now,
			AfterTo: now, Targets: []ReceiptTarget{{QueryID: -42, Verdict: "regressed",
				BeforeMeanMs: 1, AfterMeanMs: 1.8, BeforeCalls: 40, AfterCalls: 41,
				ChangePct: 80, PValue: 0.001}}, Tolerance: "missed",
			Incidents: []string{"i-1"}, LockFindings: []int64{3},
			BornQueryIDs: []BornQuery{{QueryID: 9, FirstSeen: now, MeanMs: 2, Calls: 5,
				Relations: []string{"public.orders"}}}, Confounders: []string{"none"},
			ContradictedBy: []string{"queryid:-42"}, EvidenceIDs: []string{"stmt:1"}},
		ReceiptDecidedAt: ptr(now.Add(4 * time.Minute)), ShadowAutoClear: ptr(false),
		ShadowScore: ShadowIncorrect, CreditState: CreditVoid, CreditedMinutes: 0,
		ToilModelVersion: ptr(1), CreatedAt: now, CompletedAt: ptr(now.Add(30 * time.Second)),
		UpdatedAt: now.Add(5 * time.Minute)}
}

func TestStoreRoundTripPreservesEveryField(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	in := fullReview(t)
	stored, err := st.Insert(f.ctx, in)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !uuidPattern.MatchString(stored.ID) {
		t.Fatalf("id %q is not a uuid", stored.ID)
	}
	in.ID = stored.ID
	got, err := st.Get(f.ctx, stored.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertNoZeroField(t, reflect.ValueOf(got), "Review")
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip changed the review:\n got %+v\nwant %+v", got, in)
	}
	if !reflect.DeepEqual(stored, got) {
		t.Fatal("Insert returned a review different from what Get reads")
	}
}

// assertNoZeroField fails on any zero field so the round trip covers all.
func assertNoZeroField(t *testing.T, v reflect.Value, path string) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		fv := v.Field(i)
		if fv.IsZero() && name != "CreditedMinutes" {
			t.Errorf("%s.%s is zero; the round trip does not cover it", path, name)
		}
	}
}

func TestStoreGetUnknown(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	for _, id := range []string{"not-a-uuid", "8f14e45f-ceea-467a-9575-0c9a1d8e7a01", ""} {
		if _, err := st.Get(f.ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

// completeReview inserts a complete, undecided review.
func completeReview(t *testing.T, f *fixture, st *Store, edit func(*Review)) Review {
	t.Helper()
	now := utc(time.Now())
	r := Review{Database: "testdb", Source: SourceCLI, Actor: "mcp:token:ci",
		InputHash: strings.Repeat("e", 64), State: StateComplete, Stage: StageDone,
		Verdict: VerdictReview, DeterministicVerdict: VerdictReview, ParseMode: ParseAST,
		ModelStatus: ModelNoModel, LandingState: LandingAwaiting,
		ReceiptVerdict: ReceiptPending, ShadowScore: ShadowUnscored,
		CreditState: CreditNone, CreatedAt: now, CompletedAt: &now, UpdatedAt: now}
	if edit != nil {
		edit(&r)
	}
	out, err := st.Insert(f.ctx, r)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return out
}

func TestStoreDecideOnce(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	got, err := st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAConcur, Actor: "user:1"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got.DBAVerdict != DBAConcur || got.DBAActor != "user:1" || got.DBADecidedAt == nil {
		t.Fatalf("decided review = %+v", got)
	}
	if got.CreditState != CreditPending || got.CreditedMinutes != 15 ||
		got.ToilModelVersion == nil || *got.ToilModelVersion != 1 {
		t.Fatalf("credit after concur = %s %d %v, want pending 15 v1", got.CreditState,
			got.CreditedMinutes, got.ToilModelVersion)
	}
	_, err = st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAOverride, Actor: "user:2",
		Reason: "x", Direction: DirectionTooStrict})
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second Decide = %v, want ErrAlreadyDecided", err)
	}
	again, _ := st.Get(f.ctx, r.ID)
	if again.DBAVerdict != DBAConcur || again.DBAActor != "user:1" {
		t.Fatalf("the second Decide changed the verdict: %+v", again)
	}
}

func TestStoreDecideRefusals(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	running := completeReview(t, f, st, func(r *Review) {
		r.State, r.Verdict, r.DeterministicVerdict, r.ParseMode = StateRunning, "", "", ""
		r.ModelStatus, r.LandingState, r.ReceiptVerdict, r.CompletedAt = "", "", "", nil
	})
	cases := map[string]struct {
		id   string
		v    DBAVerdict
		want error
	}{
		"override without reason": {r.ID, DBAVerdict{Verdict: DBAOverride, Actor: "user:1",
			Direction: DirectionTooStrict}, ErrInvalid},
		"override blank reason": {r.ID, DBAVerdict{Verdict: DBAOverride, Actor: "user:1",
			Reason: "   ", Direction: DirectionTooStrict}, ErrInvalid},
		"override without direction": {r.ID, DBAVerdict{Verdict: DBAOverride,
			Actor: "user:1", Reason: "x"}, ErrInvalid},
		"unknown verdict": {r.ID, DBAVerdict{Verdict: "approve", Actor: "user:1"},
			ErrInvalid},
		"unknown direction": {r.ID, DBAVerdict{Verdict: DBAConcur, Actor: "user:1",
			Direction: "sideways"}, ErrInvalid},
		"no actor": {r.ID, DBAVerdict{Verdict: DBAConcur}, ErrInvalid},
		"long reason": {r.ID, DBAVerdict{Verdict: DBAConcurWithEdits, Actor: "user:1",
			Reason: strings.Repeat("r", MaxReasonRunes+1)}, ErrInvalid},
		"running review": {running.ID, DBAVerdict{Verdict: DBAConcur, Actor: "user:1"},
			ErrInvalid},
		"unknown review": {"8f14e45f-ceea-467a-9575-0c9a1d8e7a01",
			DBAVerdict{Verdict: DBAConcur, Actor: "user:1"}, ErrNotFound},
	}
	for name, c := range cases {
		if _, err := st.Decide(f.ctx, c.id, c.v); !errors.Is(err, c.want) {
			t.Errorf("%s: Decide = %v, want %v", name, err, c.want)
		}
	}
	if got, _ := st.Get(f.ctx, r.ID); got.DBAVerdict != "" {
		t.Fatalf("a refused Decide stored %q", got.DBAVerdict)
	}
	ok, err := st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAOverride, Actor: "user:1",
		Reason: "the column is unused", Direction: DirectionTooStrict})
	if err != nil || ok.DBAReason != "the column is unused" || ok.CreditState != CreditNone {
		t.Fatalf("valid override = %+v, %v", ok, err)
	}
}

func TestStoreDecideRace(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	r := completeReview(t, f, st, nil)
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAConcur,
				Actor: "user:" + string(rune('a'+i))})
		}(i)
	}
	wg.Wait()
	ok, already := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAlreadyDecided):
			already++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || already != 9 {
		t.Fatalf("%d decided and %d ErrAlreadyDecided, want 1 and 9", ok, already)
	}
	if n := f.count(`SELECT count(*) FROM sage.change_review WHERE dba_verdict IS NOT NULL`); n != 1 {
		t.Fatalf("%d verdicts stored, want 1", n)
	}
}

func TestStoreListFilters(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	base := utc(time.Now())
	pr := "https://github.com/acme/shop/pull/1"
	for i := 0; i < 5; i++ {
		completeReview(t, f, st, func(r *Review) {
			r.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			if i%2 == 0 {
				r.PRURL = pr
			}
		})
	}
	all, err := st.List(f.ctx, ListFilter{})
	if err != nil || len(all) != 5 {
		t.Fatalf("List = %d, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].CreatedAt.After(all[i-1].CreatedAt) {
			t.Fatal("List is not newest first")
		}
	}
	byPR, _ := st.List(f.ctx, ListFilter{PRURL: pr})
	if len(byPR) != 3 {
		t.Fatalf("by pr_url = %d, want 3", len(byPR))
	}
	limited, _ := st.List(f.ctx, ListFilter{Limit: 2})
	if len(limited) != 2 || !limited[0].CreatedAt.Equal(all[0].CreatedAt) {
		t.Fatalf("limit 2 = %d rows", len(limited))
	}
	failed, _ := st.List(f.ctx, ListFilter{State: StateFailed})
	if len(failed) != 0 {
		t.Fatalf("state failed = %d, want 0", len(failed))
	}
	for _, bad := range []ListFilter{{State: "done"}, {Limit: -1}, {Limit: MaxListLimit + 1}} {
		if _, err := st.List(f.ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("List(%+v) = %v, want ErrInvalid", bad, err)
		}
	}
	if all[0].Transcript != nil {
		t.Fatal("List returned transcripts")
	}
}
