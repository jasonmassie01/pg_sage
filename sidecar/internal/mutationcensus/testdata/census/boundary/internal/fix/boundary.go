package fix

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type pool interface {
	Exec(ctx context.Context, sql string, args ...any) (any, error)
	Query(ctx context.Context, sql string, args ...any) (any, error)
}

const touchSQL = `UPDATE sage.findings SET updated_at = now() WHERE id = $1`

func child(name string) string { return name + "_p1" }

// DropChild concatenates a literal verb with a computed name: a sink.
func DropChild(ctx context.Context, p pool, x string) error {
	_, err := p.Exec(ctx, "DROP TABLE "+child(x))
	return err
}

// FormatVacuum builds the statement with fmt.Sprintf: a sink.
func FormatVacuum(ctx context.Context, p pool, table string) error {
	q := fmt.Sprintf("VACUUM (ANALYZE) %s", table)
	_, err := p.Exec(ctx, q)
	return err
}

// Hypo creates a hypothetical index: a sink.
func Hypo(ctx context.Context, p pool) error {
	_, err := p.Query(ctx, "SELECT * FROM hypopg_create_index($1)", "CREATE INDEX ON t (a)")
	return err
}

// Columns names columns that contain verbs: not a sink.
func Columns(ctx context.Context, p pool) error {
	_, err := p.Query(ctx, "SELECT created, updated_at, drop_count, analyze_count "+
		"FROM public.t WHERE created > now()")
	return err
}

// Comments hides verbs in SQL comments and Go comments: not a sink.
// DELETE FROM public.t
func Comments(ctx context.Context, p pool) error {
	_, err := p.Query(ctx, "-- DROP TABLE public.t\nSELECT 1 /* TRUNCATE public.t */")
	return err
}

// Explain runs EXPLAIN (ANALYZE): ANALYZE is not at the statement start.
func Explain(ctx context.Context, p pool) error {
	_, err := p.Query(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) SELECT 1")
	return err
}

// SageOnly writes only pg_sage's own schema: excluded.
func SageOnly(ctx context.Context, p pool) error {
	if _, err := p.Exec(ctx, "INSERT INTO sage.x (a) VALUES ($1)", 1); err != nil {
		return err
	}
	if _, err := p.Exec(ctx, touchSQL, 1); err != nil {
		return err
	}
	_, err := p.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sage;
		CREATE INDEX IF NOT EXISTS x_a ON sage.x (a);
		COMMENT ON TABLE sage.x IS 'pg_sage';
		DELETE FROM sage.x WHERE a IN (SELECT a FROM public.t)`)
	return err
}

// ParamText passes a write-looking parameter value with a read: not a sink.
func ParamText(ctx context.Context, p pool) error {
	_, err := p.Query(ctx, "SELECT $1::text", "DROP TABLE public.t")
	return err
}

// ReadIntoSage reads a user table into sage: the target is sage, excluded.
func ReadIntoSage(ctx context.Context, p pool) error {
	_, err := p.Exec(ctx, "INSERT INTO sage.snap SELECT * FROM public.t")
	return err
}

// WriteFromSage writes a user table from sage rows: a sink.
func WriteFromSage(ctx context.Context, p pool) error {
	_, err := p.Exec(ctx, "INSERT INTO public.t SELECT * FROM sage.snap")
	return err
}

// CTEDelete deletes inside a CTE: a sink.
func CTEDelete(ctx context.Context, p pool) error {
	_, err := p.Query(ctx, "WITH gone AS (DELETE FROM public.t RETURNING id) "+
		"SELECT count(*) FROM gone")
	return err
}

type store struct{ p pool }

// exec forwards its SQL to the driver: calls to it are read like Exec.
func (s *store) exec(ctx context.Context, what, q string) error {
	_, err := s.p.Exec(ctx, q)
	return err
}

// Wrapped writes through the package's wrapper: a sink.
func Wrapped(ctx context.Context, s *store) error {
	return s.exec(ctx, "truncate", "TRUNCATE public.t")
}

// BindAfter passes write-looking text as a bind value after dynamic SQL:
// not a sink.
func BindAfter(ctx context.Context, p pool, q string) error {
	_, err := p.Exec(ctx, q, "DROP TABLE public.t")
	return err
}

func roleStatement(exists bool, role string) string {
	if exists {
		return "ALTER ROLE " + role + " NOLOGIN"
	}
	return "SELECT 1"
}

// exec2 tags its SQL with a comment before forwarding it.
func (s *store) exec2(ctx context.Context, stmt string) error {
	_, err := s.p.Exec(ctx, "/* pg_sage */ "+stmt)
	return err
}

// Built writes SQL a package function returns, through a tagging
// wrapper: a sink.
func Built(ctx context.Context, s *store, role string) error {
	return s.exec2(ctx, roleStatement(true, role))
}

func sageIdent(name string) string { return pgx.Identifier{"sage", name}.Sanitize() }

func function(schema, name string) string { return pgx.Identifier{schema, name}.Sanitize() }

// SageBuilt writes sage tables named through a range and a quoting
// helper: excluded.
func SageBuilt(ctx context.Context, p pool, name string) error {
	for _, table := range []string{"sage.a", "sage.b"} {
		if _, err := p.Exec(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	_, err := p.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", sageIdent(name)))
	return err
}

// RangeMixed ranges over a sage table and a user table: a sink.
func RangeMixed(ctx context.Context, p pool) error {
	for _, table := range []string{"sage.a", "public.b"} {
		if _, err := p.Exec(ctx, "TRUNCATE "+table); err != nil {
			return err
		}
	}
	return nil
}

// QuotedHypo names hypopg_create_index through a quoting helper: a sink.
func QuotedHypo(ctx context.Context, p pool, schema string) error {
	_, err := p.Query(ctx, "SELECT * FROM "+function(schema, "hypopg_create_index")+"($1)")
	return err
}

type copier interface {
	CopyFrom(ctx context.Context, t pgx.Identifier, cols []string, src any) (int64, error)
}

// CopySage bulk-loads a sage table: excluded.
func CopySage(ctx context.Context, c copier) error {
	_, err := c.CopyFrom(ctx, pgx.Identifier{"sage", "x"}, nil, nil)
	return err
}

// CopyUser bulk-loads a user table: a sink.
func CopyUser(ctx context.Context, c copier) error {
	_, err := c.CopyFrom(ctx, pgx.Identifier{"public", "x"}, nil, nil)
	return err
}

// Batched queues a user-table write on a batch: a sink.
func Batched(b *pgx.Batch) {
	b.Queue("INSERT INTO sage.ok VALUES (1)")
	b.Queue("DELETE FROM public.q WHERE id = $1", 1)
}

// RequestCtx takes its context from a method call: a sink.
func RequestCtx(r interface{ Context() context.Context }, p pool) error {
	_, err := p.Exec(r.Context(), "DELETE FROM public.r")
	return err
}

func run(c context.Context, p pool, q string) error {
	_, err := p.Exec(c, q)
	return err
}

// ShortCtx writes through a wrapper whose context is not named ctx: a sink.
func ShortCtx(c context.Context, p pool) error {
	return run(c, p, "DROP TABLE public.c")
}

// MultiDrop names a sage table first and a user table second: a sink.
func MultiDrop(ctx context.Context, p pool) error {
	_, err := p.Exec(ctx, "DROP TABLE IF EXISTS sage.a, public.b")
	return err
}
