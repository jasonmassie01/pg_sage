package briefing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// llm-06b: the read side of sage.briefings (Store) without a database:
// limit parsing, nil stores and error propagation. The database tests are
// in store_db_test.go.
// No concurrent access tests here: ParseLimit is pure and the fakes hold
// no state; store_db_test.go reads concurrently on a real pool.

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int
		ok   bool
	}{
		{"", DefaultListLimit, true},
		{"1", 1, true},
		{"20", 20, true},
		{"100", MaxListLimit, true},
		{"0", 0, false},
		{"101", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"1.5", 0, false},
		{" 5", 0, false},
		{"99999999999999999999", 0, false},
	}
	for _, c := range cases {
		got, err := ParseLimit(c.raw)
		if c.ok {
			if err != nil || got != c.want {
				t.Errorf("ParseLimit(%q) = %d, %v; want %d, nil", c.raw, got, err, c.want)
			}
			continue
		}
		if !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("ParseLimit(%q) = %d, %v; want ErrInvalidLimit", c.raw, got, err)
		}
	}
	if DefaultListLimit != 20 || MaxListLimit != 100 {
		t.Fatalf("limits = %d/%d, want 20/100", DefaultListLimit, MaxListLimit)
	}
	if !strings.Contains(ErrInvalidLimit.Error(), "1 to 100") {
		t.Fatalf("ErrInvalidLimit %q does not name the range", ErrInvalidLimit)
	}
}

type failingQuerier struct {
	err   error
	calls int
	sql   string
	args  []any
}

func (q *failingQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.calls++
	q.sql, q.args = sql, args
	return nil, q.err
}

func TestStoreNilQuerierFailsWithoutPanicking(t *testing.T) {
	for name, store := range map[string]*Store{"nil store": nil, "nil querier": NewStore(nil)} {
		if _, err := store.List(context.Background(), 5); err == nil ||
			!strings.Contains(err.Error(), "no database") {
			t.Errorf("%s: List err = %v, want a no-database error", name, err)
		}
		_, err := store.Latest(context.Background())
		if err == nil || errors.Is(err, ErrNoBriefing) {
			t.Errorf("%s: Latest err = %v, want an error that is not ErrNoBriefing", name, err)
		}
	}
}

func TestStoreListRejectsLimitOutOfRange(t *testing.T) {
	q := &failingQuerier{}
	store := NewStore(q)
	for _, limit := range []int{0, -1, MaxListLimit + 1} {
		if _, err := store.List(context.Background(), limit); !errors.Is(err, ErrInvalidLimit) {
			t.Errorf("List(%d) err = %v, want ErrInvalidLimit", limit, err)
		}
	}
	if q.calls != 0 {
		t.Fatalf("an out-of-range limit reached the database (%d queries)", q.calls)
	}
}

func TestStoreQueryErrorIsWrappedWithContext(t *testing.T) {
	cause := errors.New("connection refused")
	q := &failingQuerier{err: cause}
	_, err := NewStore(q).List(context.Background(), 7)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "read briefings") {
		t.Fatalf("List err = %v, want it to wrap the cause with read briefings", err)
	}
	if len(q.args) != 1 || q.args[0] != 7 {
		t.Fatalf("limit was not passed as a parameter: args %v", q.args)
	}
	if !strings.Contains(q.sql, "sage.briefings") || !strings.Contains(q.sql, "$1") {
		t.Fatalf("query is not a parameterized read of sage.briefings:\n%s", q.sql)
	}
	_, err = NewStore(q).Latest(context.Background())
	if !errors.Is(err, cause) || errors.Is(err, ErrNoBriefing) {
		t.Fatalf("Latest err = %v, want the cause, not ErrNoBriefing", err)
	}
}

func TestNoBriefingReasonExplainsTheSchedule(t *testing.T) {
	for _, want := range []string{"briefing.schedule", "off"} {
		if !strings.Contains(NoBriefingReason, want) {
			t.Errorf("NoBriefingReason %q does not mention %q", NoBriefingReason, want)
		}
	}
	if ErrNoBriefing.Error() != "no briefing yet" {
		t.Fatalf("ErrNoBriefing = %q", ErrNoBriefing)
	}
}
