package explain

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// failingQuerier fails every catalog read, as a lost connection would.
type failingQuerier struct{ err error }

func (f failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, f.err
}

func (f failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingRow(f)
}

type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }

func fixedShape(q sqlast.ReadQuery, err error) func(string) (sqlast.ReadQuery, error) {
	return func(string) (sqlast.ReadQuery, error) { return q, err }
}

func TestProveReadPropagatesCatalogErrors(t *testing.T) {
	lost := errors.New("connection lost")
	shape := sqlast.ReadQuery{Functions: []sqlast.QualifiedName{{Name: "f"}}}
	got, err := ProveRead(context.Background(), failingQuerier{err: lost}, "SELECT f()",
		fixedShape(shape, nil), ReadProofOptions{})
	if !errors.Is(err, lost) {
		t.Fatalf("error = %v, want the catalog error", err)
	}
	if got != "" {
		t.Errorf("refusal = %q alongside an error, want empty", got)
	}
}

func TestProveReadRefusesWhatCannotBeInspected(t *testing.T) {
	got, err := ProveRead(context.Background(), failingQuerier{}, "SELECT 1",
		fixedShape(sqlast.ReadQuery{}, sqlast.ErrUnavailable), ReadProofOptions{})
	if err != nil {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(got, "could not inspect") {
		t.Errorf("refusal = %q, want it to say the query could not be inspected", got)
	}
}

// The deny-list applies before any catalog read, so it holds even when
// the catalog is unreachable.
func TestProveReadDenyListNeedsNoCatalog(t *testing.T) {
	shape := sqlast.ReadQuery{Functions: []sqlast.QualifiedName{
		{Schema: "sage", Name: "anything"}}}
	opts := ReadProofOptions{DenyFunction: func(n sqlast.QualifiedName) bool {
		return n.Schema == "sage"
	}}
	got, err := ProveRead(context.Background(), failingQuerier{err: errors.New("x")},
		"SELECT sage.anything()", fixedShape(shape, nil), opts)
	if err != nil {
		t.Fatalf("error = %v, want a refusal before the catalog", err)
	}
	if !strings.Contains(got, "sage.anything") {
		t.Errorf("refusal = %q, want it to name sage.anything", got)
	}
}

func TestProveReadStructuralRefusals(t *testing.T) {
	cases := []struct {
		shape sqlast.ReadQuery
		want  string
	}{
		{sqlast.ReadQuery{ModifiesData: true}, "data-modifying"},
		{sqlast.ReadQuery{LockingClause: true}, "locking"},
		{sqlast.ReadQuery{SelectInto: true}, "SELECT INTO"},
	}
	for _, c := range cases {
		got, err := ProveRead(context.Background(), failingQuerier{}, "x",
			fixedShape(c.shape, nil), ReadProofOptions{AllowRLS: true})
		if err != nil || !strings.Contains(got, c.want) {
			t.Errorf("%+v: refusal = %q, %v; want %q", c.shape, got, err, c.want)
		}
	}
}
