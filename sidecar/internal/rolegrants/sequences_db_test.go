package rolegrants

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// noFixtureSequences drops the test fixture's pg_hint_plan, whose
// hint_plan.hints id sequence would otherwise be one more schema here.
func noFixtureSequences(t *testing.T, ctx context.Context, db grantsDB) {
	t.Helper()
	if _, err := db.admin.Exec(ctx, "DROP EXTENSION IF EXISTS pg_hint_plan"); err != nil {
		t.Fatalf("drop pg_hint_plan: %v", err)
	}
}

// seqDB adds sequences to the grants database: a and "My App" hold
// sequences the role cannot read, readable holds one it was granted, and
// the sage schema's are pg_sage's own. The schemas are the first look's
// user schemas, so the two agree on what is unreadable.
func seqDB(t *testing.T) (grantsDB, context.Context) {
	t.Helper()
	db, ctx := newGrantsDB(t)
	noFixtureSequences(t, ctx, db)
	rq := pgx.Identifier{db.roleName}.Sanitize()
	for _, s := range []string{
		"CREATE SCHEMA a", "CREATE SCHEMA readable",
		"CREATE TABLE a.t (id serial)", `CREATE SEQUENCE "My App".s`,
		"CREATE SEQUENCE readable.s", "CREATE SEQUENCE sage.s",
		"GRANT USAGE ON SCHEMA a, readable TO " + rq,
		"GRANT SELECT ON SEQUENCE readable.s TO " + rq,
	} {
		if _, err := db.admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return db, ctx
}

// Integration: the schemas listed are those holding user sequences, and
// the missing ones are exactly those with a sequence the role cannot read.
// Running the SQL the check hands out makes it pass.
func TestCheckSequenceSelect(t *testing.T) {
	db, ctx := seqDB(t)
	got, err := CheckSequenceSelect(ctx, db.role)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if want := []string{`"My App"`, "a", "readable"}; !reflect.DeepEqual(got.Schemas, want) {
		t.Fatalf("schemas = %q, want %q", got.Schemas, want)
	}
	if want := []string{`"My App"`, "a"}; !reflect.DeepEqual(got.Missing, want) {
		t.Fatalf("missing = %q, want %q", got.Missing, want)
	}
	sql := got.GrantSQL(db.roleName)
	if want := `GRANT SELECT ON ALL SEQUENCES IN SCHEMA "My App", a TO ` + db.roleName; sql !=
		want {
		t.Fatalf("GrantSQL = %q, want %q", sql, want)
	}
	if _, err := db.admin.Exec(ctx, sql); err != nil {
		t.Fatalf("emitted SQL %q failed: %v", sql, err)
	}
	after, err := CheckSequenceSelect(ctx, db.role)
	if err != nil {
		t.Fatalf("check after grant: %v", err)
	}
	if after.Lacking() || len(after.Schemas) != 3 {
		t.Fatalf("after grant = %+v, want three schemas, none missing", after)
	}
}

// USAGE alone also lets the role read last_value, the way the first look
// tests readability, so it is not reported missing.
func TestCheckSequenceSelectUsageCounts(t *testing.T) {
	db, ctx := seqDB(t)
	rq := pgx.Identifier{db.roleName}.Sanitize()
	if _, err := db.admin.Exec(ctx, `GRANT USAGE ON ALL SEQUENCES IN SCHEMA a, "My App" TO `+
		rq); err != nil {
		t.Fatalf("grant usage: %v", err)
	}
	got, err := CheckSequenceSelect(ctx, db.role)
	if err != nil || got.Lacking() {
		t.Fatalf("with USAGE: %+v, %v; want nothing missing", got, err)
	}
}

// Empty: tables but no sequences means nothing to read and nothing missing.
func TestCheckSequenceSelectNoSequences(t *testing.T) {
	db, ctx := newGrantsDB(t)
	noFixtureSequences(t, ctx, db)
	got, err := CheckSequenceSelect(ctx, db.role)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if got.Lacking() || len(got.Schemas) != 0 || got.Missing != nil {
		t.Fatalf("no sequences = %+v, want empty", got)
	}
}

func TestCheckSequenceSelectErrors(t *testing.T) {
	var nilPool *pgxpool.Pool
	if _, err := CheckSequenceSelect(context.Background(), nilPool); !errors.Is(err,
		ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
	db, ctx := newGrantsDB(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err := CheckSequenceSelect(cctx, db.role)
	if err == nil || !strings.Contains(err.Error(), "sequence") {
		t.Fatalf("cancelled err = %v, want one naming the sequence check", err)
	}
}

// An extension's sequence is read by the first look's runway check like
// any other, so the probe reports it too: the guide and the first look
// agree on what is unreadable (unlike tables, whose owner pg_sage never
// needs for an extension's objects).
func TestCheckSequenceSelectCountsExtensionSequences(t *testing.T) {
	db, ctx := newGrantsDB(t)
	noFixtureSequences(t, ctx, db)
	for _, s := range []string{"CREATE SCHEMA extseq", "CREATE SEQUENCE extseq.s",
		"ALTER EXTENSION plpgsql ADD SEQUENCE extseq.s"} {
		if _, err := db.admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.admin.Exec(context.Background(),
			"ALTER EXTENSION plpgsql DROP SEQUENCE extseq.s")
	})
	got, err := CheckSequenceSelect(ctx, db.role)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if want := []string{"extseq"}; !reflect.DeepEqual(got.Missing, want) {
		t.Fatalf("missing = %q, want %q", got.Missing, want)
	}
}
