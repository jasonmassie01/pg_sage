package upgradecheck

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-02b unit tests: options, statement classification, the before
// and after comparison, the one-per-database registry and the brief hash.

// plainProvider is a provider that writes no receipts.
type plainProvider struct{ created int }

func (p *plainProvider) Create(context.Context, clone.CloneSpec) (clone.Clone, error) {
	p.created++
	return clone.Clone{ID: "x", DSN: "postgres://h/x"}, nil
}
func (p *plainProvider) Destroy(context.Context, clone.Clone) error { return nil }
func (p *plainProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, nil
}

// receiptedProvider is plainProvider that names a receipt for its clone.
type receiptedProvider struct{ plainProvider }

func (p *receiptedProvider) ReceiptOf(string) (int64, bool) { return 7, true }

func validOptions(p clone.Provider) RehearsalOptions {
	return RehearsalOptions{Database: "app", SourceMajor: 14, TargetMajor: 17,
		Provider: p, Upgrader: &fixtureRehearser{}, Production: notProductionServer,
		Top: func(context.Context, int) ([]TopQuery, error) { return nil, nil }}
}

func TestRehearsal_InvalidOptionsCreateNothing(t *testing.T) {
	cases := map[string]func(*RehearsalOptions){
		"no database":        func(o *RehearsalOptions) { o.Database = "" },
		"no provider":        func(o *RehearsalOptions) { o.Provider = nil },
		"no upgrader":        func(o *RehearsalOptions) { o.Upgrader = nil },
		"no production":      func(o *RehearsalOptions) { o.Production = nil },
		"no top source":      func(o *RehearsalOptions) { o.Top = nil },
		"zero source major":  func(o *RehearsalOptions) { o.SourceMajor = 0 },
		"downgrade":          func(o *RehearsalOptions) { o.TargetMajor = 13 },
		"absurd major":       func(o *RehearsalOptions) { o.TargetMajor = 1001 },
		"negative timeout":   func(o *RehearsalOptions) { o.StatementTimeout = -time.Second },
		"unreceipted clones": func(o *RehearsalOptions) { o.Provider = &plainProvider{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := &receiptedProvider{}
			opts := validOptions(p)
			mutate(&opts)
			_, err := Rehearse(context.Background(), opts)
			if !errors.Is(err, ErrInvalidRehearsal) {
				t.Fatalf("Rehearse = %v, want ErrInvalidRehearsal", err)
			}
			if p.created != 0 {
				t.Fatalf("an invalid rehearsal created %d clones", p.created)
			}
		})
	}
}

func TestRehearsal_TopQueryErrorCreatesNoClone(t *testing.T) {
	p := &receiptedProvider{}
	opts := validOptions(p)
	cause := errors.New("pg_stat_statements unavailable")
	opts.Top = func(context.Context, int) ([]TopQuery, error) { return nil, cause }
	rep, err := Rehearse(context.Background(), opts)
	if !errors.Is(err, cause) || p.created != 0 {
		t.Fatalf("Rehearse = %v (created %d), want the read error and no clone", err,
			p.created)
	}
	if rep.State != RehearsalFailed || !strings.Contains(rep.Error, "pg_stat_statements") {
		t.Fatalf("report = %+v, want failed with the reason", rep)
	}
}

func TestClassifyTopQuery(t *testing.T) {
	cases := []struct {
		text     string
		src, dst int
		status   string
		reason   string
	}{
		{qSkewed, 14, 17, "", ""},
		{qForUpdate, 14, 17, QuerySkipped, "lock"},
		{qInsert, 14, 17, QuerySkipped, "read"},
		{"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d", 16, 17, QuerySkipped, ""},
		{qParam, 14, 17, QueryNotComparable, "PG16"},
		{qParam, 16, 15, QueryNotComparable, "PG16"},
		{qParam, 16, 17, "", ""},
		{"SELECT '$1' AS literal", 14, 17, "", ""},
		{"", 14, 17, QuerySkipped, ""},
		{"SELECT 1; SELECT 2", 14, 17, QuerySkipped, ""},
	}
	for _, c := range cases {
		status, reason := classifyTopQuery(c.text, c.src, c.dst)
		if status != c.status || (c.status != "" && reason == "") ||
			!strings.Contains(reason, c.reason) {
			t.Errorf("classify(%q, %d, %d) = %q %q, want %q containing %q", c.text, c.src,
				c.dst, status, reason, c.status, c.reason)
		}
	}
}

func blocks(n int64) *int64 { return &n }

func TestCompareCaptures(t *testing.T) {
	q := RehearsedQuery{Status: QueryCompared,
		Before:       &PlanCapture{PlanHash: "a", BlocksPerCall: blocks(100)},
		AsCutOver:    &PlanCapture{PlanHash: "b", BlocksPerCall: blocks(2300)},
		AfterAnalyze: &PlanCapture{PlanHash: "a", BlocksPerCall: blocks(110)}}
	compareCaptures(&q)
	if !q.ChangedAsCutOver || q.ChangedAfterAnalyze || !q.RecoveredAfterAnalyze() {
		t.Fatalf("flags = %+v", q)
	}
	if *q.BlocksDeltaAsCutOver != 2200 || *q.BlocksDeltaAfterAnalyze != 10 {
		t.Fatalf("deltas = %d %d, want 2200 10", *q.BlocksDeltaAsCutOver,
			*q.BlocksDeltaAfterAnalyze)
	}
	// Planned only (no blocks) or a failed capture: no delta, no change claimed.
	q = RehearsedQuery{Status: QueryCompared, Before: &PlanCapture{PlanHash: "a"},
		AsCutOver:    &PlanCapture{Error: "timed out on the clone"},
		AfterAnalyze: &PlanCapture{PlanHash: "c"}}
	compareCaptures(&q)
	if q.ChangedAsCutOver || !q.ChangedAfterAnalyze || q.RecoveredAfterAnalyze() ||
		q.BlocksDeltaAsCutOver != nil || q.BlocksDeltaAfterAnalyze != nil {
		t.Fatalf("partial captures = %+v", q)
	}
	if !q.PlanChanged() {
		t.Fatal("a plan changed after ANALYZE is a changed plan")
	}
}

func TestSummarizeQueries(t *testing.T) {
	var rep RehearsalReport
	summarize(&rep, []RehearsedQuery{
		{QueryID: 1, Status: QueryCompared, ChangedAsCutOver: true},
		{QueryID: 2, Status: QueryCompared},
		{QueryID: 3, Status: QueryCompared, ChangedAsCutOver: true, ChangedAfterAnalyze: true},
		{QueryID: 4, Status: QuerySkipped, Reason: "locks rows"},
		{QueryID: 5, Status: QueryError, Reason: "relation does not exist"},
	})
	if len(rep.Changed) != 2 || rep.PlansChanged != 2 || rep.RecoveredAfterAnalyze != 1 ||
		rep.Unchanged != 1 || len(rep.NotCompared) != 2 {
		t.Fatalf("summary = %+v", rep)
	}
	summarize(&rep, nil)
	if rep.Note != NothingToCompare || rep.Changed != nil {
		t.Fatalf("empty summary = %+v", rep)
	}
}

func TestBriefHash_IgnoresTheRehearsal(t *testing.T) {
	b := Brief{Database: "app", Target: BriefVersion{Major: 17}, Method: PGUpgrade}
	before := briefHash(b)
	r := RehearsalReport{Database: "app", PlansChanged: 3}
	b.Rehearsal = &r
	if briefHash(b) != before {
		t.Fatal("attaching a rehearsal changed the brief's content hash")
	}
}

// Concurrency: one rehearsal per database; a second request returns the
// running one, another database runs alongside.
func TestRehearsalRegistry_OneRunPerDatabase(t *testing.T) {
	reg := NewRehearsalRegistry()
	release := make(chan struct{})
	inner := &fixtureInner{block: release, createErr: errors.New("provider down")}
	p := &blockingReceipted{inner: inner}
	var mu sync.Mutex
	var finished []RehearsalReport
	done := func(r RehearsalReport, _ error) {
		mu.Lock()
		finished = append(finished, r)
		mu.Unlock()
	}
	opts := validOptions(p)
	first, started, err := reg.Start(context.Background(), opts, done)
	if err != nil || !started {
		t.Fatalf("first Start = %v %v", started, err)
	}
	second, started, err := reg.Start(context.Background(), opts, done)
	if err != nil || started || second != first {
		t.Fatalf("second Start = %p %v %v, want the running %p", second, started, err, first)
	}
	other := opts
	other.Database = "billing"
	third, started, err := reg.Start(context.Background(), other, done)
	if err != nil || !started || third == first {
		t.Fatalf("other database Start = %v %v", started, err)
	}
	if snap := first.Snapshot(); snap.State != RehearsalRequested {
		t.Fatalf("running snapshot state = %q, want requested", snap.State)
	}
	close(release)
	<-first.Done()
	<-third.Done()
	if rep, err := first.Result(); err == nil || rep.State != RehearsalFailed {
		t.Fatalf("result = %+v %v, want the create failure", rep, err)
	}
	if _, running := reg.Running("app"); running {
		t.Fatal("a finished rehearsal is still registered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finished) != 2 {
		t.Fatalf("done called %d times, want once per run", len(finished))
	}
}

func TestRehearsalRegistry_InvalidOptionsStartNothing(t *testing.T) {
	reg := NewRehearsalRegistry()
	opts := validOptions(&receiptedProvider{})
	opts.Database = ""
	if _, started, err := reg.Start(context.Background(), opts, nil); started ||
		!errors.Is(err, ErrInvalidRehearsal) {
		t.Fatalf("Start = %v %v, want ErrInvalidRehearsal", started, err)
	}
}

// blockingReceipted is fixtureInner with a receipt name.
type blockingReceipted struct{ inner *fixtureInner }

func (b *blockingReceipted) Create(ctx context.Context, s clone.CloneSpec) (clone.Clone,
	error) {
	return b.inner.Create(ctx, s)
}
func (b *blockingReceipted) Destroy(ctx context.Context, c clone.Clone) error {
	return b.inner.Destroy(ctx, c)
}
func (b *blockingReceipted) SnapshotAge(ctx context.Context) (time.Duration, error) {
	return b.inner.SnapshotAge(ctx)
}
func (b *blockingReceipted) ReceiptOf(string) (int64, bool) { return 1, true }
