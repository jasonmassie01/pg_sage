package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// vacuum-02: a holder is cleared only when its PID is gone, its backend
// was restarted (another backend_start), or it holds neither a snapshot
// nor an XID. Before, only backend_xmin was checked, so an idle READ
// COMMITTED writer (backend_xid, no snapshot) read as cleared.
// No concurrency test: the post-check is one read of pg_stat_activity.

type idleXIDHolder struct {
	pid   int
	start time.Time
	conn  *pgxpool.Conn
}

func startIdleXIDHolder(t *testing.T, pool *pgxpool.Pool) idleXIDHolder {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	for _, sql := range []string{"CREATE TEMP TABLE vacuum02_holder (id int)",
		"BEGIN ISOLATION LEVEL READ COMMITTED", "INSERT INTO vacuum02_holder VALUES (1)"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			conn.Release()
			t.Fatalf("%s: %v", sql, err)
		}
	}
	h := idleXIDHolder{pid: int(conn.Conn().PgConn().PID()), conn: conn}
	if err := pool.QueryRow(ctx, `SELECT backend_start FROM pg_stat_activity
		WHERE pid = $1 AND backend_xid IS NOT NULL AND backend_xmin IS NULL`, h.pid).
		Scan(&h.start); err != nil {
		conn.Release()
		t.Fatalf("holder %d does not hold only an XID: %v", h.pid, err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "ROLLBACK")
		_, _ = conn.Exec(context.Background(), "DROP TABLE IF EXISTS vacuum02_holder")
		conn.Release()
	})
	return h
}

func holderEvidence(pid int, start time.Time) map[string]any {
	return map[string]any{"pid": pid, "backend_start": start.UTC().Format(time.RFC3339Nano)}
}

func TestVerifyXminBlockerFailsWhileTheBackendHoldsAnXID(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), zeroTime(), nopLog)
	holder := startIdleXIDHolder(t, pool)
	// A cancel does not end an idle transaction: the XID is still held.
	if _, err := pool.Exec(ctx, "SELECT pg_cancel_backend($1)", holder.pid); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	criterion, err := exec.verifyXminBlocker(ctx, holderEvidence(holder.pid, holder.start))
	if err == nil || criterion != "custodian_xmin_blocker" ||
		!strings.Contains(err.Error(), "still holds") {
		t.Fatalf("verify after cancel = %q, %v; want the holder to remain", criterion, err)
	}
	// Once the transaction ends the backend holds nothing: cleared.
	if _, err := holder.conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := exec.verifyXminBlocker(ctx,
		holderEvidence(holder.pid, holder.start)); err != nil {
		t.Fatalf("verify after the transaction ended: %v", err)
	}
}

func TestVerifyXminBlockerTreatsARestartedPIDAsCleared(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), zeroTime(), nopLog)
	holder := startIdleXIDHolder(t, pool)
	other := holder.start.Add(-time.Hour)
	if _, err := exec.verifyXminBlocker(ctx, holderEvidence(holder.pid, other)); err != nil {
		t.Fatalf("verify with another backend_start: %v", err)
	}
	gone := holderEvidence(2_000_000_000, holder.start)
	if _, err := exec.verifyXminBlocker(ctx, gone); err != nil {
		t.Fatalf("verify of a PID that is gone: %v", err)
	}
}

func TestVerifyXminBlockerRequiresIdentityEvidence(t *testing.T) {
	pool, ctx := requireDB(t)
	exec := New(pool, config.DefaultConfig(), zeroTime(), nopLog)
	for name, evidence := range map[string]map[string]any{
		"no pid":            {"backend_start": time.Now().Format(time.RFC3339Nano)},
		"zero pid":          {"pid": 0, "backend_start": time.Now().Format(time.RFC3339Nano)},
		"no backend_start":  {"pid": 42},
		"bad backend_start": {"pid": 42, "backend_start": "yesterday"},
		"nil evidence":      nil,
	} {
		if _, err := exec.verifyXminBlocker(ctx, evidence); err == nil {
			t.Fatalf("%s: verify accepted %#v", name, evidence)
		}
	}
}
