package changereview

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// CHECK-FND-06: Submit validation, dedupe on input_hash within 30 days,
// the bounded runner (max_concurrent_per_database plus a queue of 8),
// change_review.enabled, and interrupted rows at start.

func sqlRequest(sql string) Request {
	return Request{SQL: sql, PR: PRInfo{URL: "https://github.com/acme/shop/pull/7",
		Number: 7, HeadSHA: "abc1234"}}
}

func submitAndWait(t *testing.T, f *fixture, s *Service, r Request) Review {
	t.Helper()
	r.WaitSeconds = 20
	got, created, err := s.Submit(f.ctx, cliCaller, r)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !created {
		t.Fatalf("Submit reused %s, want a new review", got.ID)
	}
	if got.State != StateComplete {
		t.Fatalf("review %s is %s (%s), want complete", got.ID, got.State, got.FailureReason)
	}
	return got
}

func TestSubmitRefusesInvalidRequests(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(staticAnalysis(t)))
	cases := map[string]struct {
		r    Request
		want string
	}{
		"empty":        {Request{}, "sql or a diff"},
		"too much sql": {sqlRequest(strings.Repeat("x", 100001)), "max_sql_bytes (100000)"},
		"unknown tool": {Request{SQL: "SELECT 1", MigrationTool: "mybatis"}, "mybatis"},
	}
	for name, c := range cases {
		_, _, err := s.Submit(f.ctx, cliCaller, c.r)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want ErrInvalid naming %q", name, err, c.want)
		}
	}
	if n := f.count(`SELECT count(*) FROM sage.change_review`); n != 0 {
		t.Fatalf("refused requests stored %d rows", n)
	}
}

func TestSubmitCompletesDeterministicReview(t *testing.T) {
	f := newFixture(t)
	det := ruleFinding("CR-13", SeverityWarning)
	s := f.service(f.deps(staticAnalysis(t, det)))
	r := sqlRequest("ALTER TABLE public.orders ADD COLUMN note text")
	r.Diff = "diff --git a/x b/x\n+SECRET-DIFF-LINE\n"
	got := submitAndWait(t, f, s, r)
	if got.Verdict != VerdictReview || got.DeterministicVerdict != VerdictReview ||
		got.ModelStatus != ModelNoModel || got.ParseMode != ParseAST {
		t.Fatalf("review = %s/%s/%s/%s", got.Verdict, got.DeterministicVerdict,
			got.ModelStatus, got.ParseMode)
	}
	if mustJSON(t, got.Findings) != mustJSON(t, []Finding{det}) {
		t.Fatalf("findings = %s, want the deterministic finding byte-for-byte",
			mustJSON(t, got.Findings))
	}
	if got.LandingState != LandingAwaiting || got.ReceiptVerdict != ReceiptPending ||
		got.CreditState != CreditNone || got.CompletedAt == nil || got.Stage != StageDone {
		t.Fatalf("completion state = %+v", got)
	}
	if !strings.HasPrefix(got.Markdown, "<!-- pg_sage-review db=testdb id="+got.ID) {
		t.Fatalf("markdown starts %q", firstLine(got.Markdown))
	}
	if got.DiffSHA256 != DiffSHA256(r.Diff) || got.ContentHash == "" || got.InputHash == "" {
		t.Fatalf("hashes = %q %q %q", got.DiffSHA256, got.ContentHash, got.InputHash)
	}
	if n := f.count(`SELECT count(*) FROM sage.change_review c
		WHERE row_to_json(c)::text LIKE '%SECRET-DIFF-LINE%'`); n != 0 {
		t.Fatal("the raw diff was stored")
	}
	stored, err := s.Get(f.ctx, got.ID)
	if err != nil || mustJSON(t, stored) != mustJSON(t, got) {
		t.Fatalf("Get differs from Submit's result: %v", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestSubmitDedupesWithin30Days(t *testing.T) {
	f := newFixture(t)
	s := f.service(f.deps(staticAnalysis(t)))
	first := submitAndWait(t, f, s, sqlRequest("ALTER TABLE t ADD COLUMN a int"))
	again, created, err := s.Submit(f.ctx, cliCaller, sqlRequest("ALTER TABLE t ADD COLUMN a int"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat = %s created=%v %v, want %s reused", again.ID, created, err, first.ID)
	}
	other := sqlRequest("ALTER TABLE t ADD COLUMN a int")
	other.PR.HeadSHA = "abc9999"
	if r, created, _ := s.Submit(f.ctx, cliCaller, other); !created || r.ID == first.ID {
		t.Fatal("a new head_sha reused the review")
	}
	f.exec(`UPDATE sage.change_review SET created_at = now() - interval '31 days'
		WHERE id = $1`, first.ID)
	old, created, err := s.Submit(f.ctx, cliCaller, sqlRequest("ALTER TABLE t ADD COLUMN a int"))
	if err != nil || !created || old.ID == first.ID {
		t.Fatalf("after 31 days: %s created=%v %v, want a new review", old.ID, created, err)
	}
}

func TestSubmitDoesNotReuseFailedReviews(t *testing.T) {
	f := newFixture(t)
	calls := 0
	var mu sync.Mutex
	broken := analyzerFunc(func(context.Context, Input) (Analysis, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return Analysis{}, errStageBroken
	})
	s := f.service(f.deps(broken))
	r := sqlRequest("SELECT 1")
	r.WaitSeconds = 10
	a, _, err := s.Submit(f.ctx, cliCaller, r)
	if err != nil || a.State != StateFailed || !strings.Contains(a.FailureReason, "stage broken") {
		t.Fatalf("broken analyzer: %+v %v", a, err)
	}
	b, created, _ := s.Submit(f.ctx, cliCaller, r)
	if !created || b.ID == a.ID {
		t.Fatal("a failed review was reused")
	}
}

func TestSubmitBusyWhenRunnerAndQueueFull(t *testing.T) {
	f := newFixture(t)
	g := newGate()
	d := f.deps(g.analyzer(t))
	d.Config.MaxConcurrentPerDatabase = 2
	s := f.service(d)
	var ids []string
	for i := 0; i < 10; i++ {
		r, created, err := s.Submit(f.ctx, cliCaller, sqlRequest("SELECT "+strconv.Itoa(i)))
		if err != nil || !created || r.State != StateRunning {
			t.Fatalf("submit %d: %+v %v", i+1, r, err)
		}
		ids = append(ids, r.ID)
	}
	<-g.entered
	<-g.entered
	_, _, err := s.Submit(f.ctx, cliCaller, sqlRequest("SELECT 11"))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("the 11th Submit = %v, want ErrBusy", err)
	}
	if g.inside.Load() != 2 {
		t.Fatalf("%d analyses running, want 2", g.inside.Load())
	}
	close(g.release)
	for _, id := range ids {
		r, err := s.Wait(f.ctx, id, 20*time.Second)
		if err != nil || r.State != StateComplete {
			t.Fatalf("review %s: %s %v", id, r.State, err)
		}
	}
	if g.peak.Load() > 2 {
		t.Fatalf("peak concurrency %d, want at most 2", g.peak.Load())
	}
	if r, created, err := s.Submit(f.ctx, cliCaller, sqlRequest("SELECT 12")); err != nil ||
		!created {
		t.Fatalf("after the queue drained: %+v %v", r, err)
	}
}

func TestSubmitDisabled(t *testing.T) {
	f := newFixture(t)
	d := f.deps(staticAnalysis(t))
	d.Config.Enabled = false
	s := f.service(d)
	if _, _, err := s.Submit(f.ctx, cliCaller, sqlRequest("SELECT 1")); !errors.Is(err,
		ErrDisabled) {
		t.Fatalf("disabled Submit = %v", err)
	}
	if _, err := s.ReportDeployed(f.ctx, cliCaller, Deployment{PRURL: "https://x.y/1",
		DeployedAt: time.Now()}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled ReportDeployed = %v", err)
	}
}

func TestStartInterruptsStaleRunningRows(t *testing.T) {
	f := newFixture(t)
	st := NewStore(f.pool, "testdb", nil)
	stale := insertRunning(t, f, st, 6*time.Minute)
	recent := insertRunning(t, f, st, time.Minute)
	s, err := New(f.deps(staticAnalysis(t)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Start(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("Start interrupted %d (%v), want 1", n, err)
	}
	got, _ := s.Get(f.ctx, stale)
	if got.State != StateFailed || got.FailureReason != FailureInterrupted {
		t.Fatalf("stale row = %s %q", got.State, got.FailureReason)
	}
	if got, _ := s.Get(f.ctx, recent); got.State != StateRunning {
		t.Fatalf("a row running for one minute became %s", got.State)
	}
}

func insertRunning(t *testing.T, f *fixture, st *Store, age time.Duration) string {
	t.Helper()
	at := utc(time.Now().Add(-age))
	r, err := st.Insert(f.ctx, Review{Database: "testdb", Source: SourceCLI, Actor: "a",
		InputHash: strings.Repeat("f", 64), State: StateRunning, Stage: StageDeterministic,
		ShadowScore: ShadowUnscored, CreditState: CreditNone, CreatedAt: at, UpdatedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestSubmitBeforeStartIsUnavailable(t *testing.T) {
	f := newFixture(t)
	s, err := New(f.deps(staticAnalysis(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Submit(f.ctx, cliCaller, sqlRequest("SELECT 1")); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("Submit before Start = %v", err)
	}
}

func TestNewRefusesIncompleteDeps(t *testing.T) {
	f := newFixture(t)
	bad := f.deps(staticAnalysis(t))
	bad.Config.MaxConcurrentPerDatabase = 0
	for name, d := range map[string]Deps{
		"no database": {Pool: f.pool, Config: testConfig(), Analyzer: staticAnalysis(t)},
		"no pool":     {Database: "x", Config: testConfig(), Analyzer: staticAnalysis(t)},
		"no analyzer": {Database: "x", Pool: f.pool, Config: testConfig()},
		"bad config":  bad,
	} {
		if _, err := New(d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: New = %v, want ErrInvalid", name, err)
		}
	}
}
