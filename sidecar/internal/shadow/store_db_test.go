package shadow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Integration (real Postgres): shadow decisions are recorded once per
// finding fingerprint per window. While one is pending, the next cycles
// only note that pg_sage still wants the same thing (last seen, count);
// two cycles recording the same finding at once still leave one decision.

func TestRecordRoundTripsTheDecision(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	d := sample("index_create", `CREATE INDEX CONCURRENTLY i_a ON public.o (a)`)
	d.RollbackSQL = `DROP INDEX CONCURRENTLY public.i_a`
	d.TrustedDetail = "single object"
	d.DecisionID = 314
	stored := record(t, s, d)
	got := get(t, s, stored.ID)
	if got.SQL != d.SQL || got.RollbackSQL != d.RollbackSQL || got.Shape != d.Shape ||
		got.Fingerprint != d.Fingerprint || got.Family != "tuning" ||
		got.Class != "index_create" || got.FindingID != 77 || got.Database != "orders" {
		t.Fatalf("identity: %+v", got)
	}
	if got.GateVerdict != "observe_only" || got.GateReason != "autonomy_level" ||
		got.TrustedVerdict != "execute" || got.TrustedReason != "autonomy_l3" ||
		got.TrustedDetail != "single object" || got.GrantedLevel != 1 || got.DecisionID != 314 {
		t.Fatalf("gate: %+v", got)
	}
	p := got.Prediction
	if p.Metric != "mean_exec_time" || p.ExpectedChangePct == nil || *p.ExpectedChangePct != -40 ||
		len(p.TargetQueryIDs) != 1 || p.TargetQueryIDs[0] != 9100001 || p.Method != "model" {
		t.Fatalf("prediction: %+v", p)
	}
	if got.Evidence["finding_category"] != "missing_index" {
		t.Fatalf("evidence: %v", got.Evidence)
	}
	if got.Status != StatusPending || got.Score != "" || got.SeenCount != 1 ||
		got.RecordedAt.IsZero() || got.ScoredAt != nil || got.Counted {
		t.Fatalf("lifecycle: %+v", got)
	}
	if _, err := s.Get(ctx, stored.ID+1000000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing decision: %v", err)
	}
}

func TestRecordRejectsAnIncompleteDecision(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	base := sample("vacuum", `VACUUM public.o`)
	for name, mutate := range map[string]func(*Decision){
		"no fingerprint":     func(d *Decision) { d.Fingerprint = "" },
		"no class":           func(d *Decision) { d.Class = "" },
		"no family":          func(d *Decision) { d.Family = "" },
		"no sql":             func(d *Decision) { d.SQL = "  " },
		"bad gate verdict":   func(d *Decision) { d.GateVerdict = "maybe" },
		"bad trusted":        func(d *Decision) { d.TrustedVerdict = "" },
		"executing verdict":  func(d *Decision) { d.GateVerdict = "execute" },
		"level out of range": func(d *Decision) { d.GrantedLevel = 7 },
	} {
		d := base
		mutate(&d)
		if _, _, err := s.Record(ctx, d); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.shadow_decision`).
		Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after invalid records = %d (%v)", n, err)
	}
	if _, _, err := NewStore(nil).Record(ctx, base); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil pool: %v", err)
	}
}

func TestSeenDedupesPerFingerprintPerWindow(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	d := sample("analyze", `ANALYZE public.o`)
	db := 3
	d.DatabaseID = &db
	seen, err := s.Seen(ctx, &db, d.Fingerprint, 24*time.Hour)
	if err != nil || seen {
		t.Fatalf("first sighting: seen=%v err=%v", seen, err)
	}
	stored := record(t, s, d)
	age(t, pool, stored.ID, time.Hour)
	for i := 0; i < 2; i++ {
		if seen, err := s.Seen(ctx, &db, d.Fingerprint, 24*time.Hour); err != nil || !seen {
			t.Fatalf("pending decision: seen=%v err=%v", seen, err)
		}
	}
	got := get(t, s, stored.ID)
	if got.SeenCount != 3 || !got.LastSeenAt.After(got.RecordedAt) {
		t.Fatalf("pending decision not bumped: %+v", got)
	}
	// Another database's same fingerprint is its own decision.
	other := 4
	if seen, _ := s.Seen(ctx, &other, d.Fingerprint, 24*time.Hour); seen {
		t.Fatal("another database shares the dedupe")
	}
	// Scored within the window: still deduplicated, not bumped.
	mustExec(t, pool, `UPDATE sage.shadow_decision SET status = 'scored', score = 'correct',
		score_source = 'operator', scored_at = now() WHERE id = $1`, stored.ID)
	if seen, _ := s.Seen(ctx, &db, d.Fingerprint, 24*time.Hour); !seen {
		t.Fatal("a decision scored inside the window must suppress a new one")
	}
	if got := get(t, s, stored.ID); got.SeenCount != 3 {
		t.Fatalf("a scored decision was bumped: %+v", got)
	}
	// Scored and older than the window: a new decision may be recorded.
	age(t, pool, stored.ID, 25*time.Hour)
	if seen, _ := s.Seen(ctx, &db, d.Fingerprint, 24*time.Hour); seen {
		t.Fatal("window elapsed, a new decision must be allowed")
	}
	if seen, _ := s.Seen(ctx, &db, d.Fingerprint, 48*time.Hour); !seen {
		t.Fatal("inside a 48h window the old decision still counts")
	}
}

func TestConcurrentCyclesRecordOneDecision(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	d := sample("index_create", `CREATE INDEX CONCURRENTLY i_c ON public.o (c)`)
	before := decisionCount("orders", "index_create", "execute")
	const cycles = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted, errs := 0, []error{}
	for i := 0; i < cycles; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := s.Record(ctx, d)
			mu.Lock()
			defer mu.Unlock()
			if ok {
				inserted++
			}
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 || inserted != 1 {
		t.Fatalf("inserted=%d errs=%v", inserted, errs)
	}
	var rows, seen int
	if err := pool.QueryRow(ctx, `SELECT count(*), max(seen_count)
		FROM sage.shadow_decision WHERE fingerprint = $1`, d.Fingerprint).
		Scan(&rows, &seen); err != nil || rows != 1 || seen != cycles {
		t.Fatalf("rows=%d seen=%d (%v), want 1 and %d", rows, seen, err, cycles)
	}
	if got := decisionCount("orders", "index_create", "execute") - before; got != 1 {
		t.Fatalf("metric counted %d decisions, want 1", got)
	}
}

func TestListFiltersNewestFirst(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	a := record(t, s, sample("index_create", `CREATE INDEX CONCURRENTLY i1 ON public.o (a)`))
	b := record(t, s, sample("index_create", `CREATE INDEX CONCURRENTLY i2 ON public.o (b)`))
	c := record(t, s, sample("vacuum", `VACUUM public.o`))
	age(t, pool, a.ID, 3*time.Hour)
	age(t, pool, b.ID, 2*time.Hour)
	age(t, pool, c.ID, time.Hour)
	mustExec(t, pool, `UPDATE sage.shadow_decision SET status = 'scored', score = 'correct',
		score_source = 'hypopg', counted = true, scored_at = now() WHERE id = $1`, a.ID)
	all, err := s.List(ctx, Filter{})
	if err != nil || len(all) != 3 || all[0].ID != c.ID || all[2].ID != a.ID {
		t.Fatalf("all: %v (%v)", ids(all), err)
	}
	idx, _ := s.List(ctx, Filter{Class: "index_create"})
	if len(idx) != 2 || idx[0].ID != b.ID {
		t.Fatalf("by class: %v", ids(idx))
	}
	correct, _ := s.List(ctx, Filter{Score: ScoreCorrect})
	if len(correct) != 1 || correct[0].ID != a.ID || !correct[0].Counted ||
		correct[0].ScoreSource != SourceHypoPG {
		t.Fatalf("by score: %+v", correct)
	}
	pending, _ := s.List(ctx, Filter{Status: StatusPending, Limit: 1})
	if len(pending) != 1 || pending[0].ID != c.ID {
		t.Fatalf("pending, limit 1: %v", ids(pending))
	}
	for name, f := range map[string]Filter{
		"status": {Status: "done"}, "score": {Score: "great"}, "limit": {Limit: 1001},
		"negative limit": {Limit: -1}, "class": {Class: "DROP TABLE x"},
	} {
		if _, err := s.List(ctx, f); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
}

func ids(ds []Decision) []int64 {
	out := make([]int64, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func TestSummaryCountsEachClass(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	empty, err := s.Summary(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ledger summary: %+v (%v)", empty, err)
	}
	scores := []struct{ score, source string }{
		{"", ""}, {ScoreCorrect, SourceHypoPG}, {ScoreCorrect, SourceOperator},
		{ScoreIncorrect, SourceExternal}, {ScoreNeutral, SourceApplied},
		{ScoreUnscored, SourceNone},
	}
	for i, sc := range scores {
		d := record(t, s, sample("index_create",
			`CREATE INDEX CONCURRENTLY i ON public.o (c`+string(rune('a'+i))+`)`))
		if sc.score == "" {
			continue
		}
		counted := sc.source == SourceHypoPG || sc.source == SourceExternal
		mustExec(t, pool, `UPDATE sage.shadow_decision SET status = 'scored', score = $2,
			score_source = $3, counted = $4, scored_at = now() WHERE id = $1`,
			d.ID, sc.score, sc.source, counted)
	}
	record(t, s, sample("vacuum", `VACUUM public.o`))
	sum, err := s.Summary(ctx)
	if err != nil || len(sum) != 2 {
		t.Fatalf("summary: %+v (%v)", sum, err)
	}
	byClass := map[string]ClassSummary{}
	for _, c := range sum {
		byClass[c.Class] = c
	}
	idx := byClass["index_create"]
	if idx.Family != "tuning" || idx.Total != 6 || idx.Pending != 1 || idx.Correct != 2 ||
		idx.Incorrect != 1 || idx.Neutral != 1 || idx.Unscored != 1 || idx.Counted != 2 ||
		idx.LastRecordedAt == nil {
		t.Fatalf("index_create summary: %+v", idx)
	}
	if v := byClass["vacuum"]; v.Family != "hygiene" || v.Total != 1 || v.Pending != 1 {
		t.Fatalf("vacuum summary: %+v", v)
	}
}

func TestHistoryNamesTheProposalsOwnShadow(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	sql := `CREATE INDEX CONCURRENTLY i_h ON public.o (h)`
	h, err := s.History(ctx, "index_create", 77, Shape(sql))
	if err != nil || h.Summary.Total != 0 || h.ThisProposal != nil || h.Class != "index_create" {
		t.Fatalf("no history: %+v (%v)", h, err)
	}
	mine := record(t, s, sample("index_create", sql))
	record(t, s, sample("index_create", `CREATE INDEX CONCURRENTLY i_k ON public.o (k)`))
	h, err = s.History(ctx, "index_create", 77, Shape(sql))
	if err != nil || h.Summary.Total != 2 || h.Family != "tuning" || h.ThisProposal == nil ||
		h.ThisProposal.ID != mine.ID {
		t.Fatalf("history: %+v (%v)", h, err)
	}
	// Matched by shape alone when the finding id differs (re-detected).
	h, _ = s.History(ctx, "index_create", 999, Shape(`create index x on public.o (h)`))
	if h.ThisProposal == nil || h.ThisProposal.ID != mine.ID {
		t.Fatalf("by shape: %+v", h)
	}
	if _, err := s.History(ctx, "", 1, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no class: %v", err)
	}
}

func TestStoreFailsWithAClosedPool(t *testing.T) {
	pool, _ := testPool(t)
	cfg := pool.Config().Copy()
	closed, err := newClosedPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(closed)
	ctx := context.Background()
	if _, err := s.Seen(ctx, nil, "f", time.Hour); err == nil {
		t.Fatal("Seen on a closed pool returned no error")
	}
	if _, err := s.Summary(ctx); err == nil {
		t.Fatal("Summary on a closed pool returned no error")
	}
}
