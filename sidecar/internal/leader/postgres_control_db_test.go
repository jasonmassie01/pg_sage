package leader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// poolOn returns a pool on database name of the test server; the pool
// connects lazily, so a database that does not exist yet is "down".
func poolOn(t *testing.T, name, user string) *pgxpool.Pool {
	t.Helper()
	pc, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	pc.ConnConfig.Database = name
	if user != "" {
		pc.ConnConfig.User, pc.ConnConfig.Password = user, user
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("pool on %s: %v", name, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// freshDatabase creates (and drops at cleanup) a bootstrapped database.
func freshDatabase(t *testing.T, admin *pgxpool.Pool, name string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	drop := func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)",
			pgx.Identifier{name}.Sanitize()))
	}
	drop()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	pool := poolOn(t, name, "")
	t.Cleanup(func() { pool.Close(); drop() })
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap %s: %v", name, err)
	}
	return pool
}

func TestPostgresStore_LeaseRecordsControlIdentity(t *testing.T) {
	ctx := context.Background()
	pool := dbPool(t)
	st := NewPostgresStore(pool)
	scope := uniqueScope(t)
	l, held, err := st.Acquire(ctx, scope, "A", time.Minute)
	if err != nil || !held {
		t.Fatalf("acquire = %+v held=%v err=%v", l, held, err)
	}
	var datname, sysid string
	if err := pool.QueryRow(ctx, `SELECT current_database(), system_identifier::text
		FROM pg_control_system()`).Scan(&datname, &sysid); err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if l.Control.Datname != datname || l.Control.SystemIdentifier != sysid || sysid == "" {
		t.Fatalf("lease control = %+v, want %s / %s", l.Control, datname, sysid)
	}
	var rowSys *string
	var rowDB string
	if err := pool.QueryRow(ctx, `SELECT control_system_identifier::text, control_datname
		FROM sage.fleet_leader_lease WHERE scope = $1`, scope).Scan(&rowSys, &rowDB); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if rowSys == nil || *rowSys != sysid || rowDB != datname {
		t.Fatalf("row control = %v / %q, want %s / %s", rowSys, rowDB, sysid, datname)
	}
	cur, held, err := st.Acquire(ctx, scope, "B", time.Minute)
	if err != nil || held || cur.Control.Datname != datname {
		t.Fatalf("follower read = %+v held=%v err=%v, want control %s", cur, held, err,
			datname)
	}
	e := NewElector(st, scope+"-e", "E", time.Minute)
	if err := e.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if s := e.Status(); s.ControlDatname != datname || s.ControlSystemIdentifier != sysid {
		t.Fatalf("status = %+v, want control %s / %s", s, datname, sysid)
	}
}

func TestPostgresStore_RestrictedControlFunctionLeavesIdentifierEmpty(t *testing.T) {
	ctx := context.Background()
	admin := dbPool(t)
	db := freshDatabase(t, admin, "sage_leader_restricted")
	role := "sage_leader_norestrict"
	_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	stmts := []string{
		"CREATE ROLE " + role + " LOGIN PASSWORD '" + role + "'",
		"REVOKE EXECUTE ON FUNCTION pg_control_system() FROM PUBLIC",
		"GRANT USAGE ON SCHEMA sage TO " + role,
		"GRANT SELECT, INSERT, UPDATE ON sage.fleet_leader_lease TO " + role,
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role) })
	st := NewPostgresStore(poolOn(t, "sage_leader_restricted", role))
	l, held, err := st.Acquire(ctx, "restricted", "A", time.Minute)
	if err != nil || !held {
		t.Fatalf("acquire as restricted role = %+v held=%v err=%v", l, held, err)
	}
	if l.Control.SystemIdentifier != "" || l.Control.Datname != "sage_leader_restricted" {
		t.Fatalf("control = %+v, want datname only", l.Control)
	}
	var rowSys *int64
	if err := db.QueryRow(ctx, `SELECT control_system_identifier
		FROM sage.fleet_leader_lease WHERE scope = 'restricted'`).Scan(&rowSys); err != nil ||
		rowSys != nil {
		t.Fatalf("row identifier = %v err=%v, want NULL", rowSys, err)
	}
}

func TestPostgresStore_PinnedControlDownNeitherLeadsElsewhere(t *testing.T) {
	ctx := context.Background()
	admin := dbPool(t)
	scope := uniqueScope(t)
	_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS sage_leader_db1 WITH (FORCE)")
	// db1 is down (does not exist): both electors of the same YAML run only
	// against it, so neither leads, and nothing is written to db2 (admin).
	down := []*Elector{
		NewElector(NewPostgresStore(poolOn(t, "sage_leader_db1", "")), scope, "A", time.Minute),
		NewElector(NewPostgresStore(poolOn(t, "sage_leader_db1", "")), scope, "B", time.Minute),
	}
	for _, e := range down {
		err := e.Tick(ctx)
		if err == nil || !strings.Contains(err.Error(), "sage_leader_db1") ||
			!strings.Contains(err.Error(), scope) {
			t.Fatalf("tick on a down control database = %v, want an error naming it", err)
		}
		if e.IsLeader() || e.Status().LastError == "" {
			t.Fatalf("status = %+v, want a follower with the error", e.Status())
		}
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM sage.fleet_leader_lease
		WHERE scope = $1`, scope).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lease rows on db2 = %d err=%v, want 0", n, err)
	}
	// db1 recovers; the elector that starts now leads there.
	db1 := freshDatabase(t, admin, "sage_leader_db1")
	c := NewElector(NewPostgresStore(db1), scope, "C", time.Minute)
	if err := c.Tick(ctx); err != nil || !c.IsLeader() {
		t.Fatalf("tick after recovery err=%v leader=%v, want leader", err, c.IsLeader())
	}
	if s := c.Status(); s.ControlDatname != "sage_leader_db1" {
		t.Fatalf("control = %q, want sage_leader_db1", s.ControlDatname)
	}
}

func TestPostgresStore_IdentityErrorIsRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st := NewPostgresStore(dbPool(t))
	if _, _, err := st.Acquire(ctx, uniqueScope(t), "A", time.Minute); err == nil ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("acquire on a cancelled context = %v, want context.Canceled", err)
	}
	l, held, err := st.Acquire(context.Background(), uniqueScope(t), "A", time.Minute)
	if err != nil || !held {
		t.Fatalf("retry held=%v err=%v", held, err)
	}
	if l.Control.Datname == "" || l.Control.SystemIdentifier == "" {
		t.Fatalf("retry control = %+v, want datname and system identifier", l.Control)
	}
	if st.noSysid.Load() {
		t.Fatal("a cancelled round trip must not turn off the system identifier")
	}
}
