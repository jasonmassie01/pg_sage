package changereview

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The review fixes on real PostgreSQL: the landing seen at review time,
// the awaiting queue's rotation, one decision per revision group in the
// track record, budget settlement on the reservation's day, verdicts
// reaching every equal revision, one DBA page per PR and a comment that
// follows the review.

// atReviewAnalysis is staticAnalysis with the landing at review set.
func atReviewAnalysis(t *testing.T, at string) analyzerFunc {
	inner := staticAnalysis(t)
	return func(ctx context.Context, in Input) (Analysis, error) {
		a, err := inner(ctx, in)
		a.Expectations.AtReview = at
		return a, err
	}
}

func TestLandingAtReviewSetsTheLandingState(t *testing.T) {
	f := newFixture(t)
	held := submitAndWait(t, f, f.service(f.deps(atReviewAnalysis(t, AtReviewLanded))),
		sqlRequest("ALTER TABLE t ADD COLUMN IF NOT EXISTS a int"))
	if held.LandingState != LandingNotApplicable || held.CreditState != CreditNone {
		t.Fatalf("already landed at review: landing %s credit %s, want not_applicable",
			held.LandingState, held.CreditState)
	}
	todo := submitAndWait(t, f, f.service(f.deps(atReviewAnalysis(t, AtReviewUnmet))),
		sqlRequest("ALTER TABLE t ADD COLUMN b int"))
	if todo.LandingState != LandingAwaiting {
		t.Fatalf("unmet at review: landing %s, want awaiting", todo.LandingState)
	}
	var checked *time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT landing_checked_at FROM sage.change_review
		WHERE id = $1`, todo.ID).Scan(&checked); err != nil {
		t.Fatal(err)
	}
	if checked == nil || !checked.Equal(*todo.CompletedAt) {
		t.Fatalf("landing_checked_at %v, want the completion %v", checked, todo.CompletedAt)
	}
	unknown := submitAndWait(t, f, f.service(f.deps(atReviewAnalysis(t, ""))),
		sqlRequest("ALTER TABLE t ADD COLUMN c int"))
	if err := f.pool.QueryRow(f.ctx, `SELECT landing_checked_at FROM sage.change_review
		WHERE id = $1`, unknown.ID).Scan(&checked); err != nil {
		t.Fatal(err)
	}
	if unknown.LandingState != LandingAwaiting || checked != nil {
		t.Fatalf("unknown at review: landing %s checked %v, want awaiting, never checked",
			unknown.LandingState, checked)
	}
}

// The awaiting queue serves the reviews checked longest ago (never
// checked first), so more than a cycle's worth all get their turn.
func TestListAwaitingRotatesByLastCheck(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	base := utc(time.Now().Add(-time.Hour))
	var ids []string
	for i := 0; i < 4; i++ {
		r := completeReview(t, f, st, func(r *Review) {
			r.CompletedAt = ptr(base.Add(time.Duration(i) * time.Minute))
		})
		ids = append(ids, r.ID)
	}
	f.exec(`UPDATE sage.change_review SET landing_checked_at = now() - interval '5 minutes'
		WHERE id = $1`, ids[0])
	f.exec(`UPDATE sage.change_review SET landing_checked_at = now() - interval '9 minutes'
		WHERE id = $1`, ids[1])
	got, err := st.ListAwaiting(f.ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ids[2], ids[3], ids[1]}
	if g := idsOf(got); strings.Join(g, ",") != strings.Join(want, ",") {
		t.Fatalf("awaiting order %v, want never checked (by completion), then the oldest "+
			"check %v", g, want)
	}
}

func idsOf(rs []Review) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

// Revisions of a PR with equal content are one DBA decision and one
// deploy: the track record counts them once.
func TestCalibrationCountsOneDecisionPerRevisionGroup(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	pr := "https://github.com/acme/shop/pull/42"
	for i := 0; i < 3; i++ {
		completeReview(t, f, st, func(r *Review) {
			r.Verdict, r.DeterministicVerdict, r.PRURL = VerdictSafe, VerdictSafe, pr
			r.ContentHash = strings.Repeat("c", 64)
			r.DBAVerdict, r.DBAActor, r.DBADecidedAt = DBAConcur, "user:1", ptr(utc(time.Now()))
			r.LandingState, r.LandingSource = LandingLanded, LandingSourceChangeEvent
			r.LandedLower, r.LandedAt = ptr(utc(time.Now())), ptr(utc(time.Now()))
			r.ReceiptVerdict, r.ReceiptDecidedAt = ReceiptRegressed, ptr(utc(time.Now()))
		})
	}
	completeReview(t, f, st, func(r *Review) {
		r.Verdict, r.DeterministicVerdict = VerdictSafe, VerdictSafe
		r.PRURL, r.ContentHash = "https://github.com/acme/shop/pull/43", strings.Repeat("d", 64)
		r.DBAVerdict, r.DBAActor, r.DBADecidedAt = DBAConcur, "user:1", ptr(utc(time.Now()))
	})
	cal, err := st.Calibration(f.ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	safe := cal.ByVerdict[VerdictSafe]
	if cal.FalseSafe != 1 || safe.Decided != 2 || safe.Concurred != 2 || safe.Regressed != 1 ||
		safe.Receipts != 1 || cal.Reviews != 2 {
		t.Fatalf("calibration %+v (safe %+v), want two decisions, one false-safe", cal, safe)
	}
	sum, err := st.Summary(f.ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if sum.FalseSafe != 1 || sum.Concurrence[VerdictSafe].Of != 2 ||
		sum.Receipts[ReceiptRegressed] != 1 || sum.Reviews != 4 {
		t.Fatalf("summary false-safe %d concurrence %+v receipts %v reviews %d", sum.FalseSafe,
			sum.Concurrence, sum.Receipts, sum.Reviews)
	}
}

// Settle charges the day the reservation was made on, even after the UTC
// day rolled over during the provider call.
func TestBudgetSettlesOnTheReservationsDay(t *testing.T) {
	f := newFixture(t)
	c := &clock{t: time.Date(2026, 10, 22, 23, 59, 50, 0, time.UTC)}
	b := NewBudget(f.pool, 100000, 100000, c.now)
	res, err := b.Reserve(f.ctx, "user:1", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Day.Format("2006-01-02") != "2026-10-22" || res.Tokens != 3000 ||
		res.Actor != "user:1" {
		t.Fatalf("reservation %+v", res)
	}
	c.set(time.Date(2026, 10, 23, 0, 0, 10, 0, time.UTC))
	other, err := b.Reserve(f.ctx, "user:1", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(f.ctx, res, 500); err != nil {
		t.Fatal(err)
	}
	if st, _ := b.Status(f.ctx, "user:1"); st.ActorUsed != 4000 || st.DatabaseUsed != 4000 {
		t.Fatalf("the new day after a settle of yesterday's call: %+v, want 4000", st)
	}
	var yesterday int64
	if err := f.pool.QueryRow(f.ctx, `SELECT tokens FROM sage.change_review_budget_day
		WHERE day = '2026-10-22' AND actor = '*'`).Scan(&yesterday); err != nil {
		t.Fatal(err)
	}
	if yesterday != 500 {
		t.Fatalf("yesterday's total %d, want the 500 tokens used", yesterday)
	}
	if err := b.Settle(f.ctx, other, 4000); err != nil {
		t.Fatalf("settling an exact reservation: %v", err)
	}
	if err := b.Settle(f.ctx, Reservation{}, 10); err == nil {
		t.Fatal("settling a zero reservation was accepted")
	}
}

// A verdict recorded on one revision reaches the later revisions of the
// PR with the same content that had none; the credit stays with it.
func TestDecideReachesLaterEqualRevisions(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	pr := "https://github.com/acme/shop/pull/50"
	mk := func(hash string, age time.Duration) Review {
		return completeReview(t, f, st, func(r *Review) {
			r.PRURL, r.ContentHash = pr, strings.Repeat(hash, 64)
			r.CreatedAt = utc(time.Now().Add(-age))
		})
	}
	first, second, edited := mk("a", 3*time.Hour), mk("a", 2*time.Hour), mk("b", time.Hour)
	if _, err := st.Decide(f.ctx, first.ID, DBAVerdict{Verdict: DBAConcur,
		Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(f.ctx, second.ID)
	if got.DBAVerdict != DBAConcur || got.VerdictCarriedFrom != first.ID ||
		got.CreditState != CreditNone {
		t.Fatalf("the later equal revision: verdict %q from %q credit %s", got.DBAVerdict,
			got.VerdictCarriedFrom, got.CreditState)
	}
	if e, _ := st.Get(f.ctx, edited.ID); e.DBAVerdict != "" {
		t.Fatalf("an edited revision took the verdict %q", e.DBAVerdict)
	}
	if d, _ := st.Get(f.ctx, first.ID); d.CreditState != CreditPending {
		t.Fatalf("the decided review's credit %s, want pending", d.CreditState)
	}
}

// One DBA page per PR while its newest review keeps needing a DBA at the
// same or a lower verdict; a worse verdict pages again.
func TestNotifierPagesOncePerPR(t *testing.T) {
	f := newFixture(t)
	n := &recordingNotifier{}
	review := f.deps(staticAnalysis(t, ruleFinding("CR-08", SeverityWarning)))
	review.Notifier = n
	risky := f.deps(staticAnalysis(t, ruleFinding("CR-03", SeverityCritical)))
	risky.Notifier = n
	submitAndWait(t, f, f.service(review), revision("ALTER TABLE t ADD COLUMN a int", "aaaa111"))
	submitAndWait(t, f, f.service(review), revision("ALTER TABLE t ADD COLUMN b int", "aaaa222"))
	if got := len(n.calls()); got != 1 {
		t.Fatalf("two review revisions paged %d times, want 1", got)
	}
	submitAndWait(t, f, f.service(risky), revision("ALTER TABLE t DROP COLUMN c", "aaaa333"))
	submitAndWait(t, f, f.service(risky), revision("ALTER TABLE t DROP COLUMN d", "aaaa444"))
	if got := len(n.calls()); got != 2 {
		t.Fatalf("a revision turning risky paged %d times in all, want 2", got)
	}
}

// What the service hands out is the review as it stands: the comment
// shows a DBA verdict and a decided receipt recorded after completion.
func TestServiceRendersTheCurrentReview(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(staticAnalysis(t, ruleFinding("CR-03", SeverityCritical))))
	r := submitAndWait(t, f, s, sqlRequest("ALTER TABLE t DROP COLUMN email"))
	if _, err := s.Decide(f.ctx, r.ID, DBAVerdict{Verdict: DBAConcurWithEdits,
		Actor: "alice", Reason: "drop it in the contract release"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(f.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Markdown, "DBA verdict: concur with edits by alice") ||
		!strings.Contains(got.Markdown, "drop it in the contract release") {
		t.Fatalf("the comment does not show the DBA verdict:\n%s", got.Markdown)
	}
	waited, err := s.Wait(f.ctx, r.ID, time.Second)
	if err != nil || waited.Markdown != got.Markdown {
		t.Fatalf("Wait renders differently from Get: %v", err)
	}
}
