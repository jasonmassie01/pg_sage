package cases

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Integration tests on real PostgreSQL (playbooks-04): every diagnostic
// a case proposes runs read-only under a 2 s statement_timeout, returns
// at most its probe's row cap, and reads only the current database's
// sessions.

const diagApp = "case_diagnostics_test"

// diagRow is one result row keyed by column name.
type diagRow map[string]any

func diagConnect(t *testing.T, dsn, database string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.RuntimeParams["application_name"] = diagApp
	if database != "" {
		cfg.Database = database
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to %q: %v", cfg.Database, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func diagExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func diagPID(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	var pid int64
	if err := conn.QueryRow(context.Background(),
		"SELECT pg_backend_pid()::int8").Scan(&pid); err != nil {
		t.Fatalf("read backend pid: %v", err)
	}
	return pid
}

// runDiagnostic runs an action's script the way an operator would, one
// statement at a time inside BEGIN READ ONLY with statement_timeout=2s.
// The result is keyed by probe; a statement above its cap fails.
func runDiagnostic(t *testing.T, conn *pgx.Conn, action string) map[probes.ID][]diagRow {
	t.Helper()
	ctx := context.Background()
	ids := diagnosticProbes[action]
	stmts := diagnosticStatements(diagnosticSQL(action))
	if len(ids) == 0 || len(stmts) != len(ids) {
		t.Fatalf("%s: %d statements for probes %v", action, len(stmts), ids)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("%s: begin read only: %v", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '2s'"); err != nil {
		t.Fatalf("%s: set statement_timeout: %v", action, err)
	}
	out := make(map[probes.ID][]diagRow, len(ids))
	for n, id := range ids {
		rows, err := queryRows(ctx, tx, stmts[n])
		if err != nil {
			t.Fatalf("%s: %s statement failed: %v", action, id, err)
		}
		spec, _ := probes.Catalog().Spec(id)
		if len(rows) > spec.MaxRows {
			t.Fatalf("%s: %s returned %d rows, cap %d", action, id, len(rows), spec.MaxRows)
		}
		out[id] = rows
	}
	return out
}

func queryRows(ctx context.Context, tx pgx.Tx, sql string) ([]diagRow, error) {
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []diagRow
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := diagRow{}
		for i, fd := range rows.FieldDescriptions() {
			row[fd.Name] = values[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// asInt64 normalizes the integer types pgx decodes pid columns to.
func asInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	}
	return 0, false
}

// waitForCount polls a count query until it reaches want.
func waitForCount(t *testing.T, conn *pgx.Conn, want int64, sql string, args ...any) {
	t.Helper()
	var got int64
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if err := conn.QueryRow(context.Background(), sql, args...).Scan(&got); err != nil {
			t.Fatalf("poll %s: %v", sql, err)
		}
		if got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waited for %d, last saw %d: %s", want, got, sql)
}

// Integration: every diagnostic runs read-only within 2 s on this server.
func TestDiagnosticScriptsRunReadOnlyOnRealPostgres(t *testing.T) {
	conn := diagConnect(t, guardedDSN(t), "")
	for action := range diagnosticProbes {
		results := runDiagnostic(t, conn, action)
		if len(results) != len(diagnosticProbes[action]) {
			t.Fatalf("%s: results for %d probes", action, len(results))
		}
	}
	conns := runDiagnostic(t, conn, "diagnose_connection_exhaustion")
	if len(conns[probes.ConnectionSaturation]) == 0 {
		t.Fatal("connection diagnostic does not see this client session")
	}
	standby := runDiagnostic(t, conn, "diagnose_standby_conflicts")
	if rows := standby[probes.StandbyReplayState]; len(rows) != 1 ||
		rows[0]["in_recovery"] != false {
		t.Fatalf("standby diagnostic on a primary = %v, want one in_recovery=false row",
			rows)
	}
}

const fleetDatabase = "case_diag_fleet_b"

// fleetFixture opens, in database B, a lock holder with an XID, a waiter
// and a snapshot holder, and returns their pids.
func fleetFixture(t *testing.T, dsn string, admin *pgx.Conn) []int64 {
	t.Helper()
	diagExec(t, admin, "DROP DATABASE IF EXISTS "+fleetDatabase+" WITH (FORCE)")
	diagExec(t, admin, "CREATE DATABASE "+fleetDatabase)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+fleetDatabase+" WITH (FORCE)")
	})
	holder := diagConnect(t, dsn, fleetDatabase)
	diagExec(t, holder, "CREATE TABLE fleet_t (id int)")
	diagExec(t, holder, "BEGIN")
	diagExec(t, holder, "SELECT txid_current()")
	diagExec(t, holder, "LOCK TABLE fleet_t IN ACCESS EXCLUSIVE MODE")
	snapshot := diagConnect(t, dsn, fleetDatabase)
	diagExec(t, snapshot, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	diagExec(t, snapshot, "SELECT 1")
	waiter := diagConnect(t, dsn, fleetDatabase)
	diagExec(t, waiter, "SET lock_timeout = '60s'")
	pids := []int64{diagPID(t, holder), diagPID(t, snapshot), diagPID(t, waiter)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = waiter.Exec(ctx, "SELECT count(*) FROM fleet_t")
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitForCount(t, admin, 1, `SELECT count(*) FROM pg_stat_activity
		WHERE pid = $1 AND wait_event_type = 'Lock'`, pids[2])
	return pids
}

// Fleet: two databases on one cluster; database A's diagnostics name no
// session of database B, while B's own lock diagnostic does.
func TestDiagnosticsListNoSessionOfAnotherDatabase(t *testing.T) {
	dsn := guardedDSN(t)
	admin := diagConnect(t, dsn, "")
	foreign := map[int64]bool{}
	for _, pid := range fleetFixture(t, dsn, admin) {
		foreign[pid] = true
	}
	for action := range diagnosticProbes {
		for id, rows := range runDiagnostic(t, admin, action) {
			assertNoForeignPID(t, action, id, rows, foreign)
		}
	}
	inB := runDiagnostic(t, diagConnect(t, dsn, fleetDatabase), "diagnose_lock_blockers")
	rows := inB[probes.LockGraph]
	if len(rows) != 1 || !foreign[mustInt(t, rows[0], "waiter_pid")] {
		t.Fatalf("database B's lock diagnostic = %v, want its one waiter", rows)
	}
}

func assertNoForeignPID(t *testing.T, action string, id probes.ID, rows []diagRow,
	foreign map[int64]bool) {
	t.Helper()
	for _, row := range rows {
		for col, v := range row {
			if !strings.HasSuffix(col, "pid") {
				continue
			}
			if pid, ok := asInt64(v); ok && foreign[pid] {
				t.Fatalf("%s: %s lists pid %d of database %s in column %s", action, id,
					pid, fleetDatabase, col)
			}
		}
	}
}

func mustInt(t *testing.T, row diagRow, col string) int64 {
	t.Helper()
	n, ok := asInt64(row[col])
	if !ok {
		t.Fatalf("column %s = %#v (%T), want an integer", col, row[col], row[col])
	}
	return n
}

// rowsWith returns the rows whose column equals want.
func rowsWith(rows []diagRow, col string, want any) []diagRow {
	var out []diagRow
	for _, row := range rows {
		if fmt.Sprint(row[col]) == fmt.Sprint(want) {
			out = append(out, row)
		}
	}
	return out
}

// The connection diagnostic counts every slot a normal role cannot use:
// superuser_reserved_connections plus, on PG16+, reserved_connections.
func TestConnectionDiagnosticCountsReservedSlots(t *testing.T) {
	conn := diagConnect(t, guardedDSN(t), "")
	var want int64
	if err := conn.QueryRow(context.Background(), `SELECT
		current_setting('superuser_reserved_connections')::int8
		+ COALESCE(current_setting('reserved_connections', true)::int8, 0)`).
		Scan(&want); err != nil {
		t.Fatalf("read the reserved slots: %v", err)
	}
	rows := runDiagnostic(t, conn, "diagnose_connection_exhaustion")[probes.ConnectionSaturation]
	if len(rows) == 0 {
		t.Fatal("connection diagnostic returned no rows")
	}
	for _, row := range rows {
		if got := mustInt(t, row, "reserved_connections"); got != want {
			t.Fatalf("reserved_connections = %d, want %d", got, want)
		}
	}
}
