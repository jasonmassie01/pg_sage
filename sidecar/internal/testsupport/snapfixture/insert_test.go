package snapfixture_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// Integration: InsertLegacy's statement runs against the real sage.snapshots
// table in its callers' database tests (internal/snapstore golden and bytes
// tests); here a recording Execer pins what it sends.

type execCall struct {
	sql  string
	args []any
}

type recordingExecer struct {
	calls  []execCall
	failOn string // category whose insert fails
	err    error
}

func (r *recordingExecer) Exec(_ context.Context, sql string,
	args ...any) (pgconn.CommandTag, error) {
	r.calls = append(r.calls, execCall{sql: sql, args: args})
	if r.failOn != "" && len(args) > 1 && args[1] == r.failOn {
		return pgconn.CommandTag{}, r.err
	}
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func TestInsertLegacyWritesOneRowPerDocument(t *testing.T) {
	cycles := generate(t, scenario())
	c := cycles[2]
	db := &recordingExecer{}
	if err := snapfixture.InsertLegacy(context.Background(), db, c); err != nil {
		t.Fatal(err)
	}
	if len(db.calls) != len(c.Docs) {
		t.Fatalf("calls = %d, want one per document (%d)", len(db.calls), len(c.Docs))
	}
	for k, call := range db.calls {
		if !strings.Contains(call.sql, "INSERT INTO sage.snapshots") ||
			!strings.Contains(call.sql, "VALUES ($1, $2, $3)") {
			t.Errorf("call %d sql = %q, want a parameterized snapshots insert", k, call.sql)
		}
		if len(call.args) != 3 || call.args[0] != c.At || call.args[1] != c.Docs[k].Category ||
			string(call.args[2].([]byte)) != string(c.Docs[k].Data) {
			t.Errorf("call %d args = %v, want (%v, %s, data)", k, call.args[:2], c.At,
				c.Docs[k].Category)
		}
	}
}

func TestInsertLegacyStopsAtTheFirstFailure(t *testing.T) {
	sentinel := errors.New("connection reset")
	cycles := generate(t, scenario())
	db := &recordingExecer{failOn: "indexes", err: sentinel}
	err := snapfixture.InsertLegacy(context.Background(), db, cycles[0])
	if !errors.Is(err, sentinel) ||
		!strings.Contains(err.Error(), "insert legacy indexes snapshot") {
		t.Fatalf("err = %v, want the wrapped failure naming the category", err)
	}
	// Documents are sorted: config_data, foreign_keys, then indexes fails.
	if len(db.calls) != 3 {
		t.Errorf("calls = %d, want 3 (nothing after the failure)", len(db.calls))
	}
}

func TestInsertLegacyEmptyCycleWritesNothing(t *testing.T) {
	db := &recordingExecer{}
	if err := snapfixture.InsertLegacy(context.Background(), db, snapfixture.Cycle{}); err != nil {
		t.Fatal(err)
	}
	if len(db.calls) != 0 {
		t.Errorf("an empty cycle made %d calls", len(db.calls))
	}
}
