package explain

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// recordingQuerier counts catalog round trips and fails each with err.
type recordingQuerier struct {
	calls atomic.Int32
	err   error
}

func (r *recordingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	r.calls.Add(1)
	return nil, r.err
}

func (r *recordingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	r.calls.Add(1)
	return errRow{r.err}
}

type errRow struct{ err error }

func (e errRow) Scan(...any) error { return e.err }

// Empty and comment-only text is refused before any catalog query, on
// both builds (the parser sees zero statements; without cgo it is absent).
func TestAnalyzeRefusalEmptyInputMakesNoRoundTrips(t *testing.T) {
	for _, q := range []string{"", "   ", "-- only a comment", "/* only a comment */"} {
		rec := &recordingQuerier{err: errors.New("unexpected catalog query")}
		ex := New(nil, liveConfig(5000), noopLogFn)
		got := ex.analyzeRefusal(context.Background(), rec, q)
		if !strings.HasPrefix(got, "could not inspect the query: ") {
			t.Errorf("analyzeRefusal(%q) = %q, want an inspection refusal", q, got)
		}
		if n := rec.calls.Load(); n != 0 {
			t.Errorf("analyzeRefusal(%q) made %d catalog round trips, want 0", q, n)
		}
		// A nil pool panics on any server contact.
		if _, err := ex.Explain(context.Background(), ExplainRequest{Query: q}); !errors.Is(
			err, ErrExplainInvalidRequest) {
			t.Errorf("Explain(%q) error = %v, want ErrExplainInvalidRequest", q, err)
		}
	}
}

// A failed implicit-cast lookup refuses ANALYZE and names what failed.
func TestAnalyzeRefusalImplicitCastLookupFailsClosed(t *testing.T) {
	rec := &recordingQuerier{err: errors.New("permission denied for table pg_cast")}
	ex := New(nil, liveConfig(5000), noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{Relations: names(qn("public", "t"))}, nil
	}
	got := ex.analyzeRefusal(context.Background(), rec, "SELECT * FROM public.t")
	for _, want := range []string{"could not verify the query against the catalog",
		"implicit casts", "permission denied for table pg_cast"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal = %q, want it to mention %q", got, want)
		}
	}
}

// A failed default-argument lookup refuses ANALYZE and names what failed.
func TestAnalyzeRefusalDefaultLookupFailsClosed(t *testing.T) {
	q := &scriptedQuerier{failOn: "proargdefaults",
		err: errors.New("canceling statement due to statement timeout")}
	ex := New(nil, liveConfig(5000), noopLogFn)
	ex.inspect = func(string) (sqlast.ReadQuery, error) {
		return sqlast.ReadQuery{Functions: names(qn("", "f"))}, nil
	}
	got := ex.analyzeRefusal(context.Background(), q, "SELECT f()")
	for _, want := range []string{"could not verify the query against the catalog",
		"default arguments", "statement timeout"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal = %q, want it to mention %q", got, want)
		}
	}
}

// scriptedQuerier answers every catalog query with an empty, safe result
// except the one whose text contains failOn, which fails with err.
type scriptedQuerier struct {
	failOn string
	err    error
}

func (s *scriptedQuerier) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, s.failOn) {
		return nil, s.err
	}
	return &safeRows{sql: sql}, nil
}

func (s *scriptedQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, s.failOn) {
		return errRow{s.err}
	}
	return errRow{pgx.ErrNoRows}
}

// safeRows yields one row per function resolution query saying "f" has
// one safe match, and no rows for anything else.
type safeRows struct {
	pgx.Rows
	sql  string
	done bool
}

func (r *safeRows) Next() bool {
	if r.done || !strings.Contains(r.sql, "provolatile NOT IN") {
		return false
	}
	r.done = true
	return true
}

func (r *safeRows) Scan(dest ...any) error {
	*dest[0].(*string), *dest[1].(*string) = "", "f"
	*dest[2].(*int), *dest[3].(*int) = 1, 0
	return nil
}

func (r *safeRows) Close()     {}
func (r *safeRows) Err() error { return nil }

// A literal-only query names nothing an implicit cast could apply to, so
// it is proven without a catalog read; naming a relation reads pg_cast.
func TestImplicitCastCheckSkipsLiteralOnlyQueries(t *testing.T) {
	g := &analyzeGuard{q: &recordingQuerier{err: errors.New("catalog read")}}
	rec := g.q.(*recordingQuerier)
	if got, err := g.checkImplicitCasts(context.Background(), sqlast.ReadQuery{}); got != "" ||
		err != nil || rec.calls.Load() != 0 {
		t.Errorf("literal-only: reason=%q err=%v reads=%d, want none", got, err,
			rec.calls.Load())
	}
	for _, shape := range []sqlast.ReadQuery{
		{Functions: names(qn("", "f"))}, {AttributeCalls: names(qn("", "a"))},
		{Operators: names(qn("", "="))}, {Types: names(qn("", "t"))},
		{Relations: names(qn("", "r"))},
	} {
		before := rec.calls.Load()
		_, err := g.checkImplicitCasts(context.Background(), shape)
		if err == nil || !strings.Contains(err.Error(), "read implicit casts: catalog read") {
			t.Errorf("%+v: err = %v, want the failed pg_cast read", shape, err)
		}
		if rec.calls.Load() != before+1 {
			t.Errorf("%+v: %d reads, want 1", shape, rec.calls.Load()-before)
		}
	}
}
