package extschema

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// No concurrent access tests: every function is pure or reads through the
// querier it is given; nothing is shared between calls.

type fakeRow struct {
	vals []string
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		*d.(*string) = r.vals[i]
	}
	return nil
}

type fakeQuerier struct {
	row  fakeRow
	sqls []string
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.sqls = append(q.sqls, sql)
	return q.row
}

func TestRelQuotesEachIdentifier(t *testing.T) {
	cases := []struct{ schema, name, want string }{
		{"public", "pg_stat_statements", `"public"."pg_stat_statements"`},
		{"ext", "pg_stat_statements_info", `"ext"."pg_stat_statements_info"`},
		{`ext"x`, "pg_stat_statements", `"ext""x"."pg_stat_statements"`},
		{"a.b", "pgstattuple_approx", `"a.b"."pgstattuple_approx"`},
		{"Ext Schema", "hypopg_reset", `"Ext Schema"."hypopg_reset"`},
	}
	for _, c := range cases {
		got, err := Rel(c.schema, c.name)
		if err != nil || got != c.want {
			t.Errorf("Rel(%q, %q) = %q, %v; want %q", c.schema, c.name, got, err, c.want)
		}
		got, err = Func(c.schema, c.name)
		if err != nil || got != c.want {
			t.Errorf("Func(%q, %q) = %q, %v; want %q", c.schema, c.name, got, err, c.want)
		}
	}
}

// An absent extension is ErrExtensionAbsent and the SQLSTATE the bare
// name raised, never "".name or a bare name.
func TestRelEmptySchemaIsAbsent(t *testing.T) {
	cases := []struct {
		fn   func(string, string) (string, error)
		code string
	}{{Rel, "42P01"}, {Func, "42883"}}
	for _, c := range cases {
		got, err := c.fn("", "pg_stat_statements")
		if got != "" {
			t.Errorf("name = %q, want empty", got)
		}
		if !errors.Is(err, ErrExtensionAbsent) {
			t.Fatalf("err = %v, want ErrExtensionAbsent", err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != c.code {
			t.Fatalf("err = %v, want SQLSTATE %s", err, c.code)
		}
		if !strings.Contains(err.Error(), "pg_stat_statements") {
			t.Errorf("err %q does not name the object", err)
		}
	}
}

func TestRelRefusesEmptyName(t *testing.T) {
	for _, fn := range []func(string, string) (string, error){Rel, Func} {
		got, err := fn("ext", "")
		if err == nil || got != "" || errors.Is(err, ErrExtensionAbsent) {
			t.Fatalf("empty name = %q, %v; want a non-absent error", got, err)
		}
	}
}

func TestDetectReadsAllThreeSchemas(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{vals: []string{"ext", "public", ""}}}
	got, err := Detect(context.Background(), q)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	want := Schemas{Statements: "ext", Pgstattuple: "public"}
	if got != want {
		t.Fatalf("Detect = %+v, want %+v", got, want)
	}
	if len(q.sqls) != 1 || !strings.Contains(q.sqls[0], "pg_catalog.pg_extension") {
		t.Fatalf("Detect ran %q, want one pg_catalog.pg_extension statement", q.sqls)
	}
}

// Error propagation: a failed catalog read names the query and is never
// reported as an absent extension.
func TestDetectFailureIsNotAbsent(t *testing.T) {
	boom := errors.New("connection reset")
	q := &fakeQuerier{row: fakeRow{err: boom}}
	got, err := Detect(context.Background(), q)
	if !errors.Is(err, boom) || errors.Is(err, ErrExtensionAbsent) {
		t.Fatalf("err = %v, want the cause and not ErrExtensionAbsent", err)
	}
	if !strings.Contains(err.Error(), "pg_extension") || got != (Schemas{}) {
		t.Fatalf("Detect = %+v, %v; want zero schemas and the query named", got, err)
	}
	rel, err := StatementsRel(context.Background(), q, "pg_stat_statements")
	if !errors.Is(err, boom) || errors.Is(err, ErrExtensionAbsent) || rel != "" {
		t.Fatalf("StatementsRel = %q, %v; want the detect error", rel, err)
	}
}

func TestDetectNilQuerier(t *testing.T) {
	if got, err := Detect(context.Background(), nil); err == nil || got != (Schemas{}) {
		t.Fatalf("Detect(nil) = %+v, %v; want an error", got, err)
	}
}

func TestStatementsRel(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{vals: []string{"ext", "", ""}}}
	got, err := StatementsRel(context.Background(), q, "pg_stat_statements_info")
	if err != nil || got != `"ext"."pg_stat_statements_info"` {
		t.Fatalf("StatementsRel = %q, %v", got, err)
	}
	q = &fakeQuerier{row: fakeRow{vals: []string{"", "public", "public"}}}
	got, err = StatementsRel(context.Background(), q, "pg_stat_statements")
	if !errors.Is(err, ErrExtensionAbsent) || got != "" {
		t.Fatalf("StatementsRel without the extension = %q, %v; want absent", got, err)
	}
}
