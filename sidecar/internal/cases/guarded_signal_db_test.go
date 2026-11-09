package cases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration tests on real PostgreSQL: the guarded cancel/terminate
// scripts the lock playbook renders signal exactly the recorded session
// and nothing else.

const guardedVictimRole = "guarded_signal_victim"

func guardedDSN(t *testing.T) string {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL unset: the guarded signal script needs real PostgreSQL")
	}
	return dsn
}

func guardedConnect(t *testing.T, dsn, user, password string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.RuntimeParams["application_name"] = "guarded_signal_test"
	if user != "" {
		cfg.User, cfg.Password = user, password
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect as %q: %v", cfg.User, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// ensureVictimRole creates a LOGIN, non-superuser role: the terminate
// script refuses superusers, and the CI user is one.
func ensureVictimRole(t *testing.T, admin *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	_, err := admin.Exec(ctx, `DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+guardedVictimRole+`') THEN
    CREATE ROLE `+guardedVictimRole+` LOGIN NOSUPERUSER PASSWORD 'victim';
  END IF;
END $$`)
	if err != nil {
		t.Fatalf("create victim role: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			"DROP ROLE IF EXISTS "+guardedVictimRole); err != nil {
			t.Errorf("drop victim role: %v", err)
		}
	})
}

type observedBackend struct {
	blocker IncidentBlocker
	query   string
}

// rcaQuerySHA is rca.blockerFromDetail's hash: sha256 of the first 200
// characters (analyzer.leftChars), first 8 bytes, hex.
func rcaQuerySHA(query string) string {
	r := []rune(query)
	if len(r) > 200 {
		query = string(r[:200])
	}
	sum := sha256.Sum256([]byte(query))
	return hex.EncodeToString(sum[:8])
}

func observeBackend(t *testing.T, admin *pgx.Conn, pid uint32, state string) observedBackend {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ob observedBackend
		err := admin.QueryRow(ctx, `SELECT pid, backend_start, query_start, state, query
			FROM pg_stat_activity WHERE pid = $1 AND state = $2`, int(pid), state).Scan(
			&ob.blocker.PID, &ob.blocker.BackendStart, &ob.blocker.QueryStart,
			&ob.blocker.State, &ob.query)
		if err == nil {
			ob.blocker.QuerySHA = rcaQuerySHA(ob.query)
			return ob
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("observe backend %d: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d never reached state %q", pid, state)
	return observedBackend{}
}

// runScript executes a rendered script and returns how many sessions it
// signalled.
func runScript(t *testing.T, admin *pgx.Conn, script string) int {
	t.Helper()
	rows, err := admin.Query(context.Background(), script)
	if err != nil {
		t.Fatalf("run guarded script: %v\n%s", err, script)
	}
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("guarded script rows: %v", err)
	}
	return n
}

func scriptFor(t *testing.T, severity Severity, b IncidentBlocker, actionType string) string {
	t.Helper()
	c := candidateOf(t, ProjectIncident(lockIncident(severity, b)), actionType)
	if c.BlockedReason != "" || c.ProposedSQL == "" {
		t.Fatalf("%s blocked for %+v: %q", actionType, b, c.BlockedReason)
	}
	return c.ProposedSQL
}

// The query is over 200 characters with multi-byte runes, so the SQL
// hash must truncate by characters exactly like RCA does.
var sleeperQuery = "SELECT pg_sleep(30) /* guarded signal test " +
	strings.Repeat("é", 220) + " */"

func TestGuardedCancelScriptSignalsOnlyTheRecordedSession(t *testing.T) {
	dsn := guardedDSN(t)
	admin := guardedConnect(t, dsn, "", "")
	sleeper := guardedConnect(t, dsn, "", "")
	done := make(chan error, 1)
	go func() {
		_, err := sleeper.Exec(context.Background(), sleeperQuery)
		done <- err
	}()
	ob := observeBackend(t, admin, sleeper.PgConn().PID(), "active")

	var sqlSHA string
	if err := admin.QueryRow(context.Background(),
		`SELECT left(encode(sha256(convert_to(left(query, 200), 'UTF8')), 'hex'), 16)
		   FROM pg_stat_activity WHERE pid = $1`, ob.blocker.PID).Scan(&sqlSHA); err != nil {
		t.Fatalf("SQL query hash: %v", err)
	}
	if sqlSHA != ob.blocker.QuerySHA {
		t.Fatalf("SQL hash %s != RCA hash %s", sqlSHA, ob.blocker.QuerySHA)
	}

	staleStart := ob.blocker
	staleStart.BackendStart = staleStart.BackendStart.Add(time.Microsecond)
	staleSHA := ob.blocker
	staleSHA.QuerySHA = "0000000000000000"
	staleQuery := ob.blocker
	staleQuery.QueryStart = staleQuery.QueryStart.Add(-time.Second)
	for name, b := range map[string]IncidentBlocker{
		"backend_start": staleStart, "query_sha": staleSHA, "query_start": staleQuery,
	} {
		if n := runScript(t, admin, scriptFor(t, SeverityWarning, b, "cancel_backend")); n != 0 {
			t.Fatalf("mismatched %s signalled %d sessions", name, n)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("sleeper ended after mismatched scripts: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if n := runScript(t, admin, scriptFor(t, SeverityWarning, ob.blocker,
		"cancel_backend")); n != 1 {
		t.Fatalf("exact identity signalled %d sessions, want 1", n)
	}
	assertQueryCancelled(t, done)
}

func assertQueryCancelled(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("sleeper error = %v, want query_canceled (57014)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sleeper was not cancelled within 5s")
	}
}

// State transition on real PG: an idle-in-transaction blocker is ended by
// the guarded terminate only while it stays idle on the recorded statement,
// and never when it runs as a superuser.
func TestGuardedTerminateScriptEndsOnlyTheIdleSession(t *testing.T) {
	dsn := guardedDSN(t)
	admin := guardedConnect(t, dsn, "", "")
	ensureVictimRole(t, admin)
	ctx := context.Background()

	super := guardedConnect(t, dsn, "", "")
	if _, err := super.Exec(ctx, "BEGIN; SELECT 1"); err != nil {
		t.Fatalf("open superuser transaction: %v", err)
	}
	superOb := observeBackend(t, admin, super.PgConn().PID(), "idle in transaction")
	if n := runScript(t, admin, scriptFor(t, SeverityWarning, superOb.blocker,
		"terminate_backend")); n != 0 {
		t.Fatalf("terminate signalled a superuser session (%d rows)", n)
	}

	victim := guardedConnect(t, dsn, guardedVictimRole, "victim")
	if _, err := victim.Exec(ctx, "BEGIN; SELECT 1"); err != nil {
		t.Fatalf("open victim transaction: %v", err)
	}
	ob := observeBackend(t, admin, victim.PgConn().PID(), "idle in transaction")
	stale := ob.blocker
	stale.QueryStart = stale.QueryStart.Add(-time.Millisecond)
	if n := runScript(t, admin, scriptFor(t, SeverityWarning, stale,
		"terminate_backend")); n != 0 {
		t.Fatalf("stale query_start signalled %d sessions", n)
	}
	if n := runScript(t, admin, scriptFor(t, SeverityWarning, ob.blocker,
		"terminate_backend")); n != 1 {
		t.Fatalf("exact idle identity signalled %d sessions, want 1", n)
	}
	assertSessionGone(t, admin, ob.blocker.PID)
	if _, err := victim.Exec(ctx, "SELECT 1"); err == nil {
		t.Fatal("victim connection still usable after terminate")
	}
}

func assertSessionGone(t *testing.T, admin *pgx.Conn, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := admin.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE pid = $1", pid).Scan(&n); err != nil {
			t.Fatalf("count backend %d: %v", pid, err)
		}
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d still present 5s after terminate", pid)
}
