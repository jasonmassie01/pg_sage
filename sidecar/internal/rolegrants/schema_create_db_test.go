package rolegrants

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// grantsDB is a fresh database holding user tables in app, "My App" and
// "order" (a reserved word), plus an empty schema, a schema whose only
// table belongs to an extension, and a sage schema. role is a login role
// that owns app.owned and has USAGE, but not CREATE, on the three schemas.
type grantsDB struct {
	admin, role *pgxpool.Pool
	roleName    string
}

func newGrantsDB(t *testing.T) (grantsDB, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "rolegrants")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rg_role_%06x", time.Now().UnixNano()&0xffffff)
	rq := pgx.Identifier{name}.Sanitize()
	for _, s := range []string{
		"CREATE SCHEMA app", `CREATE SCHEMA "My App"`, `CREATE SCHEMA "order"`,
		"CREATE SCHEMA empty_schema", "CREATE SCHEMA extonly", "CREATE SCHEMA sage",
		"CREATE TABLE app.owned (a int, b int)", `CREATE TABLE "My App".t (a int)`,
		`CREATE TABLE "order".t (a int)`, "CREATE TABLE extonly.t (a int)",
		"CREATE TABLE sage.t (a int)", "ALTER EXTENSION plpgsql ADD TABLE extonly.t",
		"CREATE ROLE " + rq + " LOGIN PASSWORD 'pw_" + name + "'",
		"ALTER TABLE app.owned OWNER TO " + rq,
		`GRANT USAGE ON SCHEMA app, "My App", "order" TO ` + rq,
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = admin.Exec(bg, "ALTER EXTENSION plpgsql DROP TABLE extonly.t")
		_, _ = admin.Exec(bg, "DROP OWNED BY "+rq)
		_, _ = admin.Exec(bg, "DROP ROLE IF EXISTS "+rq)
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword(name, "pw_"+name)
	role, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", name, err)
	}
	t.Cleanup(role.Close)
	return grantsDB{admin: admin, role: role, roleName: name}, ctx
}

// Integration: the schemas listed are exactly those holding user tables
// (not the empty one, not the extension's, not sage), quoted as SQL needs.
func TestCheckSchemaCreateWithoutGrant(t *testing.T) {
	db, ctx := newGrantsDB(t)
	got, err := CheckSchemaCreate(ctx, db.role)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	wantSchemas := []string{`"My App"`, "app", `"order"`}
	if !reflect.DeepEqual(got.Schemas, wantSchemas) {
		t.Fatalf("schemas = %q, want %q", got.Schemas, wantSchemas)
	}
	if !reflect.DeepEqual(got.Missing, wantSchemas) || !got.Lacking() {
		t.Fatalf("missing = %q, want every schema", got.Missing)
	}
	want := `GRANT CREATE ON SCHEMA "My App", app, "order" TO ` + db.roleName
	if sql := got.GrantSQL(db.roleName); sql != want {
		t.Fatalf("GrantSQL = %q, want %q", sql, want)
	}
}

// Running the emitted SQL is what makes the check pass: the SQL is valid
// for quoted and reserved schema names, and the check reads it live.
func TestCheckSchemaCreateAfterGrant(t *testing.T) {
	db, ctx := newGrantsDB(t)
	if _, err := db.admin.Exec(ctx, "GRANT CREATE ON SCHEMA app TO "+db.roleName); err != nil {
		t.Fatalf("grant app: %v", err)
	}
	partial, err := CheckSchemaCreate(ctx, db.role)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !reflect.DeepEqual(partial.Missing, []string{`"My App"`, `"order"`}) {
		t.Fatalf("missing = %q, want the two schemas without CREATE", partial.Missing)
	}
	if _, err := db.admin.Exec(ctx, partial.GrantSQL(db.roleName)); err != nil {
		t.Fatalf("emitted SQL %q failed: %v", partial.GrantSQL(db.roleName), err)
	}
	full, err := CheckSchemaCreate(ctx, db.role)
	if err != nil {
		t.Fatalf("check after grant: %v", err)
	}
	if full.Lacking() || len(full.Missing) != 0 || len(full.Schemas) != 3 {
		t.Fatalf("after grant = %+v, want nothing missing", full)
	}
}

// Why the grant exists: owning the table is not enough for CREATE INDEX or
// CREATE STATISTICS; CREATE on the table's schema is.
func TestTableOwnerNeedsSchemaCreate(t *testing.T) {
	db, ctx := newGrantsDB(t)
	stmts := []string{"CREATE INDEX rg_owned_a ON app.owned (a)",
		"CREATE STATISTICS app.rg_owned_ab (dependencies) ON a, b FROM app.owned"}
	for _, s := range stmts {
		_, err := db.role.Exec(ctx, s)
		if err == nil || !strings.Contains(err.Error(), "permission denied for schema app") {
			t.Fatalf("%s without CREATE: err = %v, want permission denied for schema", s, err)
		}
	}
	if _, err := db.admin.Exec(ctx, "GRANT CREATE ON SCHEMA app TO "+db.roleName); err != nil {
		t.Fatalf("grant: %v", err)
	}
	for _, s := range stmts {
		if _, err := db.role.Exec(ctx, s); err != nil {
			t.Fatalf("%s with CREATE: %v", s, err)
		}
	}
}

func TestCheckSchemaCreateSuperuserLacksNothing(t *testing.T) {
	db, ctx := newGrantsDB(t)
	got, err := CheckSchemaCreate(ctx, db.admin)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if got.Lacking() || len(got.Schemas) != 3 {
		t.Fatalf("superuser = %+v, want three schemas, none missing", got)
	}
}

// Empty: a database without user tables needs CREATE nowhere yet.
func TestCheckSchemaCreateNoTables(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "rolegrants_empty")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	got, err := CheckSchemaCreate(ctx, pool)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(got.Schemas) != 0 || got.Lacking() {
		t.Fatalf("empty database = %+v, want no schemas", got)
	}
}

func TestCheckSchemaCreateErrors(t *testing.T) {
	if _, err := CheckSchemaCreate(context.Background(), nil); !errors.Is(err, ErrNoPool) {
		t.Fatalf("nil pool err = %v, want ErrNoPool", err)
	}
	db, _ := newGrantsDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CheckSchemaCreate(ctx, db.role)
	if err == nil || errors.Is(err, ErrNoPool) ||
		!strings.Contains(err.Error(), "read schema CREATE privileges") {
		t.Fatalf("cancelled err = %v, want a wrapped read error", err)
	}
}
