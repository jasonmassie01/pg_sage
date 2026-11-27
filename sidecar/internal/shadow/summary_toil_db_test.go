package shadow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// earned-04: a class's potential minutes are the current toil model's
// base minutes over its decisions that are counted and correct; pending,
// incorrect and uncounted decisions add nothing, and a class without a
// toil model has none (nil), never a default.
// No concurrent access tests: the summary is one read-only query pair.

// scoreAs scores decision id directly.
func scoreAs(t *testing.T, pool *pgxpool.Pool, id int64, score, source string, counted bool) {
	t.Helper()
	mustExec(t, pool, `UPDATE sage.shadow_decision SET status = 'scored', score = $2,
		score_source = $3, counted = $4, scored_at = now() WHERE id = $1`,
		id, score, source, counted)
}

// seedIndexCreates records index_create decisions: 4 correct (3 counted),
// 1 incorrect counted, 1 neutral counted and 1 pending.
func seedIndexCreates(t *testing.T, pool *pgxpool.Pool, s *Store) {
	t.Helper()
	plan := []struct {
		score, source string
		counted       bool
	}{
		{ScoreCorrect, SourceHypoPG, true}, {ScoreCorrect, SourceExternal, true},
		{ScoreCorrect, SourceHypoPG, true}, {ScoreCorrect, SourceOperator, false},
		{ScoreIncorrect, SourceHypoPG, true}, {ScoreNeutral, SourceExternal, true},
		{"", "", false},
	}
	for i, p := range plan {
		d := record(t, s, sample("index_create",
			`CREATE INDEX CONCURRENTLY t ON public.o (c`+string(rune('a'+i))+`)`))
		if p.score != "" {
			scoreAs(t, pool, d.ID, p.score, p.source, p.counted)
		}
	}
}

func summaryByClass(t *testing.T, s *Store, class string) map[string]ClassSummary {
	t.Helper()
	sum, err := s.SummaryFor(context.Background(), class)
	if err != nil {
		t.Fatalf("SummaryFor(%q): %v", class, err)
	}
	out := map[string]ClassSummary{}
	for _, c := range sum {
		out[c.Class] = c
	}
	return out
}

func TestPotentialMinutesEqualsTheSQLSumOverCountedCorrect(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	seedIndexCreates(t, pool, s)
	var want, base float64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(tm.base_minutes), 0)::float8,
		max(tm.base_minutes)::float8
		FROM sage.shadow_decision d JOIN sage.toil_model tm
		  ON tm.action_type = 'create_index_concurrently' AND tm.effective_to IS NULL
		WHERE d.action_class = 'index_create' AND d.counted AND d.score = 'correct'`).
		Scan(&want, &base); err != nil {
		t.Fatal(err)
	}
	idx := summaryByClass(t, s, "")["index_create"]
	if idx.PotentialMinutes == nil {
		t.Fatalf("index_create potential minutes nil: %+v", idx)
	}
	if float64(*idx.PotentialMinutes) != want || want != 3*base || base <= 0 {
		t.Fatalf("potential = %d, SQL sum = %v (base %v), want 3 x base",
			*idx.PotentialMinutes, want, base)
	}
	if idx.Total != 7 || idx.Correct != 4 || idx.Counted != 5 || idx.Pending != 1 ||
		idx.Incorrect != 1 || idx.Neutral != 1 {
		t.Fatalf("counts changed: %+v", idx)
	}
}

// Pending, incorrect, neutral and uncounted-correct decisions add 0: a
// modelled class with none counted correct has 0 potential minutes, not nil.
func TestPotentialMinutesIgnoresPendingIncorrectAndUncounted(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	plan := []struct {
		score, source string
		counted       bool
	}{
		{"", "", false}, {ScoreIncorrect, SourceHypoPG, true},
		{ScoreNeutral, SourceExternal, true}, {ScoreCorrect, SourceOperator, false},
		{ScoreCorrect, SourceApplied, false}, {ScoreUnscored, SourceNone, false},
	}
	for i, p := range plan {
		d := record(t, s, sample("index_create",
			`CREATE INDEX CONCURRENTLY z ON public.o (c`+string(rune('a'+i))+`)`))
		if p.score != "" {
			scoreAs(t, pool, d.ID, p.score, p.source, p.counted)
		}
	}
	idx := summaryByClass(t, s, "index_create")["index_create"]
	if idx.PotentialMinutes == nil || *idx.PotentialMinutes != 0 || idx.Total != 6 {
		t.Fatalf("index_create = %+v (potential %v), want modelled 0", idx,
			idx.PotentialMinutes)
	}
}

// A class with no toil model is nil however many decisions are correct.
func TestPotentialMinutesNilForAnUnmodelledClass(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	d := record(t, s, sample("vacuum", `VACUUM public.unmodelled`))
	mustExec(t, pool, `UPDATE sage.shadow_decision SET action_class = 'unclassified'
		WHERE id = $1`, d.ID)
	scoreAs(t, pool, d.ID, ScoreCorrect, SourceExternal, true)
	got := summaryByClass(t, s, "")["unclassified"]
	if got.Total != 1 || got.Correct != 1 || got.Counted != 1 {
		t.Fatalf("unclassified summary = %+v", got)
	}
	if got.PotentialMinutes != nil {
		t.Fatalf("unmodelled class potential = %d, want nil", *got.PotentialMinutes)
	}
}

// The current model prices the minutes: a retired model is not used, a
// newer version replaces it, and a class whose model is retired with no
// successor has no potential minutes.
func TestPotentialMinutesUsesTheCurrentToilModel(t *testing.T) {
	pool, _ := testPool(t)
	s := NewStore(pool)
	for _, sql := range []string{`ANALYZE public.a`, `ANALYZE public.b`} {
		d := record(t, s, sample("analyze", sql))
		scoreAs(t, pool, d.ID, ScoreCorrect, SourceExternal, true)
	}
	d := record(t, s, sample("vacuum", `VACUUM public.v`))
	scoreAs(t, pool, d.ID, ScoreCorrect, SourceExternal, true)
	restore := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.toil_model
			WHERE action_type = 'analyze_table' AND model_version = 990`)
		_, _ = pool.Exec(context.Background(), `UPDATE sage.toil_model SET effective_to = NULL
			WHERE action_type IN ('analyze_table', 'vacuum_table') AND model_version = 1`)
	}
	t.Cleanup(restore)
	mustExec(t, pool, `UPDATE sage.toil_model SET effective_to = now()
		WHERE action_type IN ('analyze_table', 'vacuum_table') AND model_version = 1`)
	mustExec(t, pool, `INSERT INTO sage.toil_model (action_type, model_version, base_minutes)
		VALUES ('analyze_table', 990, 7.5)`)
	byClass := summaryByClass(t, s, "")
	if a := byClass["analyze"]; a.PotentialMinutes == nil || *a.PotentialMinutes != 15 {
		t.Fatalf("analyze potential = %v, want 2 x 7.5 = 15", a.PotentialMinutes)
	}
	if v := byClass["vacuum"]; v.Correct != 1 || v.PotentialMinutes != nil {
		t.Fatalf("vacuum with a retired model = %+v, want nil potential", v)
	}
}

// ?class= narrows the summary: an unknown but well-formed class is an
// empty summary, a malformed one is ErrInvalid.
func TestSummaryForNarrowsToTheClass(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	seedIndexCreates(t, pool, s)
	record(t, s, sample("vacuum", `VACUUM public.o`))
	bogus, err := s.SummaryFor(ctx, "bogus")
	if err != nil || bogus == nil || len(bogus) != 0 {
		t.Fatalf("SummaryFor(bogus) = %+v (%v), want [] and no error", bogus, err)
	}
	only := summaryByClass(t, s, "vacuum")
	if len(only) != 1 || only["vacuum"].Total != 1 {
		t.Fatalf("SummaryFor(vacuum) = %+v", only)
	}
	if all := summaryByClass(t, s, ""); len(all) != 2 {
		t.Fatalf("SummaryFor(\"\") = %+v, want both classes", all)
	}
	for _, bad := range []string{"DROP TABLE", "Index_Create", "a;b"} {
		if _, err := s.SummaryFor(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("SummaryFor(%q) err = %v, want ErrInvalid", bad, err)
		}
	}
}

// A toil model that cannot be read fails the summary with its cause; it
// never falls back to minutes nobody modelled.
func TestSummaryFailsWhenTheToilModelCannotBeRead(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	seedIndexCreates(t, pool, s)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`ALTER TABLE IF EXISTS sage.toil_model_hidden RENAME TO toil_model`)
	})
	mustExec(t, pool, `ALTER TABLE sage.toil_model RENAME TO toil_model_hidden`)
	_, err := s.SummaryFor(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "toil") {
		t.Fatalf("SummaryFor with no toil model: err = %v, want a toil read error", err)
	}
}

func TestSummaryForWithoutAPoolIsUnavailable(t *testing.T) {
	var nilStore *Store
	if _, err := nilStore.SummaryFor(context.Background(), ""); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("nil store err = %v, want ErrUnavailable", err)
	}
	if _, err := NewStore(nil).SummaryFor(context.Background(), "vacuum"); !errors.Is(err,
		ErrUnavailable) {
		t.Fatalf("no-pool store err = %v, want ErrUnavailable", err)
	}
}
