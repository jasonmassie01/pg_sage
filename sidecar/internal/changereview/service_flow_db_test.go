package changereview

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The review flow: the model stage can only raise, the notifier hears
// about reviews that need a DBA once, stage failures and timeouts are
// typed, a DBA verdict carries over to a revision with the same content
// (CHECK-FND-04), deploy reports land a review once.

func TestModelStageRaisesNeverLowers(t *testing.T) {
	f := newFixture(t)
	n := &recordingNotifier{}
	d := f.deps(staticAnalysis(t))
	d.Notifier = n
	d.Reasoner = reasonerFunc(func(_ context.Context, _ Input, a Analysis) (Reasoning, error) {
		return Reasoning{ModelStatus: ModelOK, Tokens: 900, Intent: "expand",
			Findings: []Finding{modelFinding(SeverityCritical, a.Evidence[0].ID)}}, nil
	})
	s := f.service(d)
	got := submitAndWait(t, f, s, sqlRequest("ALTER TABLE t ADD COLUMN b int"))
	if got.Verdict != VerdictReview || got.DeterministicVerdict != VerdictSafe ||
		got.ModelStatus != ModelOK || got.Tokens != 900 || got.Summary.Intent != "expand" {
		t.Fatalf("review = %s/%s/%s tokens %d", got.Verdict, got.DeterministicVerdict,
			got.ModelStatus, got.Tokens)
	}
	if len(got.Findings) != 1 || got.Findings[0].Severity != SeverityWarning {
		t.Fatalf("model finding = %+v, want a warning", got.Findings)
	}
	calls := n.calls()
	if len(calls) != 1 || calls[0].ID != got.ID || calls[0].Verdict != VerdictReview {
		t.Fatalf("notifier calls = %d, want one for %s", len(calls), got.ID)
	}
}

func TestNotifierSilentForSafe(t *testing.T) {
	f := newFixture(t)
	n := &recordingNotifier{}
	d := f.deps(staticAnalysis(t))
	d.Notifier = n
	s := f.service(d)
	got := submitAndWait(t, f, s, sqlRequest("ALTER TABLE t ADD COLUMN c int"))
	if got.Verdict != VerdictSafe || len(n.calls()) != 0 {
		t.Fatalf("safe review %s notified %d times", got.Verdict, len(n.calls()))
	}
}

func TestReasonerErrorKeepsDeterministicVerdict(t *testing.T) {
	f := newFixture(t)
	d := f.deps(staticAnalysis(t, ruleFinding("CR-01", SeverityCritical)))
	d.Reasoner = reasonerFunc(func(context.Context, Input, Analysis) (Reasoning, error) {
		return Reasoning{}, errStageBroken
	})
	got := submitAndWait(t, f, f.service(d), sqlRequest("ALTER TABLE t ALTER a TYPE text"))
	if got.ModelStatus != ModelError || got.Verdict != VerdictRisky {
		t.Fatalf("review = %s %s, want error and risky", got.ModelStatus, got.Verdict)
	}
}

func TestStageTimeouts(t *testing.T) {
	f := newFixture(t)
	slow := analyzerFunc(func(ctx context.Context, _ Input) (Analysis, error) {
		<-ctx.Done()
		return Analysis{}, ctx.Err()
	})
	d := f.deps(slow)
	d.DeterministicTimeout = 50 * time.Millisecond
	s := f.service(d)
	r := sqlRequest("SELECT 1")
	r.WaitSeconds = 10
	got, _, err := s.Submit(f.ctx, cliCaller, r)
	if err != nil || got.State != StateFailed || !strings.Contains(got.FailureReason,
		"deterministic stage timed out") {
		t.Fatalf("slow analyzer: %s %q %v", got.State, got.FailureReason, err)
	}
	d = f.deps(staticAnalysis(t))
	d.ModelTimeout = 50 * time.Millisecond
	d.Reasoner = reasonerFunc(func(ctx context.Context, _ Input, _ Analysis) (Reasoning,
		error) {
		<-ctx.Done()
		return Reasoning{}, ctx.Err()
	})
	got = submitAndWait(t, f, f.service(d), sqlRequest("SELECT 2"))
	if got.ModelStatus != ModelTimeout || got.Verdict != VerdictSafe {
		t.Fatalf("slow model: %s %s, want timeout and the deterministic safe", got.ModelStatus,
			got.Verdict)
	}
}

func TestInvalidAnalysisFailsTheReview(t *testing.T) {
	f := newFixture(t)
	bad := analyzerFunc(func(context.Context, Input) (Analysis, error) {
		return Analysis{ParseMode: "guess"}, nil
	})
	s := f.service(f.deps(bad))
	r := sqlRequest("SELECT 1")
	r.WaitSeconds = 10
	got, _, _ := s.Submit(f.ctx, cliCaller, r)
	if got.State != StateFailed || !strings.Contains(got.FailureReason, "parse mode") {
		t.Fatalf("invalid analysis: %s %q", got.State, got.FailureReason)
	}
}

func revision(sql, head string) Request {
	r := sqlRequest(sql)
	r.PR.HeadSHA = head
	return r
}

func TestVerdictCarriesOverOnEqualContent(t *testing.T) {
	f := newFixture(t)
	n := &recordingNotifier{}
	d := f.deps(staticAnalysis(t, ruleFinding("CR-08", SeverityWarning)))
	d.Notifier = n
	s := f.service(d)
	first := submitAndWait(t, f, s, revision("ALTER TABLE t ADD COLUMN a int;", "aaa1111"))
	if _, err := s.Decide(f.ctx, first.ID, DBAVerdict{Verdict: DBAConcur,
		Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	second := submitAndWait(t, f, s, revision("alter table t  add column a int ; -- same",
		"bbb2222"))
	if second.RevisionOf != first.ID {
		t.Fatalf("revision_of = %q, want %s", second.RevisionOf, first.ID)
	}
	if second.DBAVerdict != DBAConcur || second.VerdictCarriedFrom != first.ID ||
		second.DBAActor != "user:1" || second.CreditState != CreditNone {
		t.Fatalf("carried = %q from %q by %q credit %s", second.DBAVerdict,
			second.VerdictCarriedFrom, second.DBAActor, second.CreditState)
	}
	if len(n.calls()) != 1 {
		t.Fatalf("notifier called %d times; the carried review must not ask again",
			len(n.calls()))
	}
	edited := submitAndWait(t, f, s, revision("ALTER TABLE t ADD COLUMN a bigint;", "ccc3333"))
	if edited.DBAVerdict != "" || edited.VerdictCarriedFrom != "" {
		t.Fatalf("an edited statement carried %q", edited.DBAVerdict)
	}
	if edited.RevisionOf != second.ID {
		t.Fatalf("revision_of = %q, want the newest earlier review %s", edited.RevisionOf,
			second.ID)
	}
}

func TestCarryOverUsesHunkFilter(t *testing.T) {
	f := newFixture(t)
	d := f.deps(staticAnalysis(t, ruleFinding("CR-08", SeverityWarning)))
	d.HunkFilter = func(diff string) string {
		var keep []string
		for _, l := range strings.Split(diff, "\n") {
			if strings.Contains(l, "models/") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	s := f.service(d)
	a := revision("ALTER TABLE t ADD COLUMN a int;", "aaa1111")
	a.Diff = "+++ b/app/models/order.rb\n+++ b/README.md readme v1"
	first := submitAndWait(t, f, s, a)
	if _, err := s.Decide(f.ctx, first.ID, DBAVerdict{Verdict: DBAConcur,
		Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	b := revision("ALTER TABLE t ADD COLUMN a int;", "bbb2222")
	b.Diff = "+++ b/app/models/order.rb\n+++ b/README.md readme v2"
	if got := submitAndWait(t, f, s, b); got.VerdictCarriedFrom != first.ID {
		t.Fatal("a README-only change broke the carry-over with a hunk filter")
	}
	c := revision("ALTER TABLE t ADD COLUMN a int;", "ccc3333")
	c.Diff = "+++ b/app/models/order.rb changed\n+++ b/README.md readme v2"
	if got := submitAndWait(t, f, s, c); got.DBAVerdict != "" {
		t.Fatal("a changed data-access hunk carried the verdict")
	}
}

func TestReportDeployed(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(staticAnalysis(t)))
	r := submitAndWait(t, f, s, sqlRequest("ALTER TABLE t ADD COLUMN d int"))
	at := utc(time.Now().Add(-time.Minute))
	got, err := s.ReportDeployed(f.ctx, cliCaller, Deployment{
		PRURL: "https://github.com/acme/shop/pull/7", DeployedAt: at, Commit: "abc1234"})
	if err != nil {
		t.Fatalf("ReportDeployed: %v", err)
	}
	if got.ID != r.ID || got.LandingState != LandingLanded ||
		got.LandingSource != LandingSourceReported || !got.LandedAt.Equal(at) {
		t.Fatalf("deployed = %+v", got)
	}
	if _, err := s.ReportDeployed(f.ctx, cliCaller, Deployment{ID: r.ID,
		DeployedAt: at}); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second report = %v", err)
	}
	for name, dep := range map[string]Deployment{
		"future":      {ID: r.ID, DeployedAt: time.Now().Add(time.Hour)},
		"zero time":   {ID: r.ID},
		"neither":     {DeployedAt: at},
		"bad commit":  {ID: r.ID, DeployedAt: at, Commit: "XYZ"},
		"bad pr url":  {PRURL: "ftp://x", DeployedAt: at},
		"long before": {ID: r.ID, DeployedAt: r.CreatedAt.Add(-31 * 24 * time.Hour)},
	} {
		if _, err := s.ReportDeployed(f.ctx, cliCaller, dep); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.ReportDeployed(f.ctx, cliCaller, Deployment{
		PRURL: "https://github.com/acme/shop/pull/404", DeployedAt: at}); !errors.Is(err,
		ErrNotFound) {
		t.Fatalf("unknown PR = %v", err)
	}
}

func TestWaitReturnsRunningAfterTimeout(t *testing.T) {
	f := newFixture(t)
	g := newGate()
	s := f.service(f.deps(g.analyzer(t)))
	r, _, err := s.Submit(f.ctx, cliCaller, sqlRequest("SELECT 1"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := s.Wait(f.ctx, r.ID, 200*time.Millisecond)
	if err != nil || got.State != StateRunning || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("Wait = %s after %v (%v)", got.State, time.Since(start), err)
	}
	close(g.release)
	got, err = s.Wait(f.ctx, r.ID, 20*time.Second)
	if err != nil || got.State != StateComplete {
		t.Fatalf("Wait after release = %s %v", got.State, err)
	}
	if _, err := s.Wait(f.ctx, "8f14e45f-ceea-467a-9575-0c9a1d8e7a01", time.Second); !errors.Is(
		err, ErrNotFound) {
		t.Fatalf("Wait on an unknown id = %v", err)
	}
}

func TestRedactAndAutoClearHooks(t *testing.T) {
	f := newFixture(t)
	d := f.deps(staticAnalysis(t))
	d.Redact = func(s string) string { return strings.ReplaceAll(s, "s3cret", "[redacted]") }
	d.AutoClear = func(r Review) bool { return r.Verdict == VerdictSafe }
	s := f.service(d)
	r := sqlRequest("CREATE ROLE app PASSWORD 's3cret'")
	r.PR.Title = "rotate s3cret"
	got := submitAndWait(t, f, s, r)
	if strings.Contains(mustJSON(t, got), "s3cret") {
		t.Fatal("the stored review contains the secret")
	}
	if n := f.count(`SELECT count(*) FROM sage.change_review c
		WHERE row_to_json(c)::text LIKE '%s3cret%'`); n != 0 {
		t.Fatal("the row contains the secret")
	}
	if got.ShadowAutoClear == nil || !*got.ShadowAutoClear {
		t.Fatalf("shadow_auto_clear = %v, want true from the hook", got.ShadowAutoClear)
	}
}

func TestServiceListAndSummary(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(staticAnalysis(t, ruleFinding("CR-03", SeverityCritical))))
	a := submitAndWait(t, f, s, sqlRequest("ALTER TABLE t DROP COLUMN a"))
	submitAndWait(t, f, s, sqlRequest("ALTER TABLE t DROP COLUMN b"))
	list, err := s.List(f.ctx, ListFilter{Limit: 1})
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d %v", len(list), err)
	}
	if _, err := s.Decide(f.ctx, a.ID, DBAVerdict{Verdict: DBAConcur, Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(f.ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Reviews != 2 || sum.VerdictMix[VerdictRisky] != 2 ||
		sum.DBAVerdictMix[DBAConcur] != 1 || sum.HoursPending != 0.25 {
		t.Fatalf("summary = %+v", sum)
	}
}
