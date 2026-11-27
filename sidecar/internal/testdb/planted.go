package testdb

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlantedFunctions are the PL/pgSQL functions PlantedPathDatabase plants in
// public; the operators run the last four.
var PlantedFunctions = []string{
	"lower", "planted_texteq", "planted_nameeq", "planted_nameeqtext", "planted_domaineq",
}

// plantedSQL plants a public.lower(text) and public.= operators that win
// resolution over pg_catalog's on the database's path (public, pg_catalog):
// text = text, name = name, name = text, and one over two text domains.
// Each delegates to pg_catalog under its own pinned path, so it is counted
// (track_functions = 'pl') and returns the right answer.
var plantedSQL = []string{
	plantedFunc("lower", "a text", "text", "pg_catalog.lower(a)"),
	plantedFunc("planted_texteq", "a text, b text", "boolean", "pg_catalog.texteq(a, b)"),
	plantedFunc("planted_nameeq", "a name, b name", "boolean", "pg_catalog.nameeq(a, b)"),
	plantedFunc("planted_nameeqtext", "a name, b text", "boolean",
		"pg_catalog.nameeqtext(a, b)"),
	`CREATE DOMAIN public.planted_label AS text`,
	`CREATE DOMAIN public.planted_tag AS text`,
	plantedFunc("planted_domaineq", "a public.planted_label, b public.planted_tag",
		"boolean", "pg_catalog.texteq(a, b)"),
	plantedOp("text", "text", "planted_texteq"),
	plantedOp("name", "name", "planted_nameeq"),
	plantedOp("name", "text", "planted_nameeqtext"),
	plantedOp("public.planted_label", "public.planted_tag", "planted_domaineq"),
}

func plantedFunc(name, args, returns, body string) string {
	return "CREATE FUNCTION public." + name + "(" + args + ") RETURNS " + returns +
		" LANGUAGE plpgsql VOLATILE SET search_path = pg_catalog, pg_temp" +
		" AS $$BEGIN RETURN " + body + "; END$$"
}

func plantedOp(left, right, fn string) string {
	return "CREATE OPERATOR public.= (LEFTARG = " + left + ", RIGHTARG = " + right +
		", FUNCTION = public." + fn + ")"
}

// plantedControlSQL calls every planted function exactly once on a session
// that keeps the database's path (the positive control).
const plantedControlSQL = `SELECT lower('X'::text), 'a'::text = 'b'::text,
	'a'::name = 'b'::name, 'a'::name = 'b'::text,
	'a'::planted_label = 'b'::planted_tag`

// PlantedPath is a database whose path resolves planted public objects
// before pg_catalog's, with function call counting on.
type PlantedPath struct {
	DSN   string
	Admin *pgxpool.Pool // pinned by a startup parameter: never calls a plant
}

// PlantedPathDatabase creates a database with the planted objects, its
// search_path set to public, pg_catalog (as ALTER DATABASE or ALTER ROLE
// by an attacker would) and track_functions = 'pl'.
func PlantedPathDatabase(t testing.TB, label string) *PlantedPath {
	t.Helper()
	dsn := CreateDatabase(t, label)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog, pg_temp"
	cfg.MaxConns = 2
	admin, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	db := pgx.Identifier{cfg.ConnConfig.Database}.Sanitize()
	stmts := append(append([]string{}, plantedSQL...),
		"ALTER DATABASE "+db+" SET search_path = public, pg_catalog",
		"ALTER DATABASE "+db+" SET track_functions = 'pl'")
	execAll(t, admin, stmts)
	return &PlantedPath{DSN: dsn, Admin: admin}
}

// AssertNoPlantedCalls flushes the statistics of every idle session of
// pools (pg_sage's), then runs the positive control once on a fresh
// session with the database's path, and requires each planted function to
// show exactly the control's one call: any more ran in pg_sage's sessions.
// It also requires that no object depends on a planted one.
func (p *PlantedPath) AssertNoPlantedCalls(t testing.TB, pools ...*pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, pool := range pools {
		if err := FlushIdleSessions(ctx, pool); err != nil {
			t.Fatalf("flush pg_sage sessions: %v", err)
		}
	}
	p.runControl(t, ctx)
	calls := p.plantedCalls(t, ctx)
	var wrong []string
	for _, fn := range PlantedFunctions {
		if calls[fn] != 1 {
			wrong = append(wrong, fmt.Sprintf("%s=%d", fn, calls[fn]))
		}
	}
	if len(wrong) > 0 {
		t.Errorf("planted function calls %s, want exactly the control's 1 each: "+
			"pg_sage resolved a planted object", strings.Join(wrong, " "))
	}
	if n := p.dependents(t, ctx); n != 0 {
		t.Errorf("%d objects depend on a planted function or operator, want 0", n)
	}
}

func (p *PlantedPath) runControl(t testing.TB, ctx context.Context) {
	t.Helper()
	conn, err := pgx.Connect(ctx, p.DSN)
	if err != nil {
		t.Fatalf("control session: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var path string
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&path); err != nil ||
		path != "public, pg_catalog" {
		t.Fatalf("control session path %q (%v), want public, pg_catalog", path, err)
	}
	if _, err := conn.Exec(ctx, plantedControlSQL); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	if err := FlushStats(ctx, conn); err != nil {
		t.Fatalf("flush control session: %v", err)
	}
}

func (p *PlantedPath) plantedCalls(t testing.TB, ctx context.Context) map[string]int64 {
	t.Helper()
	calls := map[string]int64{}
	if _, err := p.Admin.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
		t.Fatalf("clear stats snapshot: %v", err)
	}
	rows, err := p.Admin.Query(ctx, `SELECT funcname::text, calls FROM pg_stat_user_functions
		WHERE schemaname = 'public' AND funcname::text = ANY($1)`, PlantedFunctions)
	if err != nil {
		t.Fatalf("read pg_stat_user_functions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			t.Fatalf("scan pg_stat_user_functions: %v", err)
		}
		calls[name] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_stat_user_functions: %v", err)
	}
	return calls
}

// dependents counts objects depending on a planted function or operator
// (one whose function is planted; extensions in public, such as pgvector,
// have = operators of their own), apart from each planted operator's
// dependency on its function.
func (p *PlantedPath) dependents(t testing.TB, ctx context.Context) int {
	t.Helper()
	var n int
	err := p.Admin.QueryRow(ctx, `WITH planted AS (
		SELECT 'pg_catalog.pg_proc'::regclass AS cls, f.oid FROM pg_proc f
		  JOIN pg_namespace s ON s.oid = f.pronamespace
		 WHERE s.nspname = 'public' AND f.proname::text = ANY($1)
		UNION ALL
		SELECT 'pg_catalog.pg_operator'::regclass, o.oid FROM pg_operator o
		  JOIN pg_proc f ON f.oid = o.oprcode
		  JOIN pg_namespace s ON s.oid = f.pronamespace
		 WHERE s.nspname = 'public' AND f.proname::text = ANY($1))
		SELECT count(*) FROM pg_depend d JOIN planted r
		    ON r.cls = d.refclassid AND r.oid = d.refobjid
		 WHERE NOT EXISTS (SELECT 1 FROM planted o
		    WHERE o.cls = d.classid AND o.oid = d.objid)`, PlantedFunctions).Scan(&n)
	if err != nil {
		t.Fatalf("read pg_depend: %v", err)
	}
	return n
}
