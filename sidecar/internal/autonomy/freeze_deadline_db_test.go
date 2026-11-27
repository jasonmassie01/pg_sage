package autonomy

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// vacuum-03 on real PostgreSQL: a red table's freeze proposal carries a
// policy deadline ahead of now and within the 100-year ceiling, even with
// the multixact rate at the 0.001/s floor (whose deadline overflowed
// time.Duration before the fix), and its evidence names the threat and
// the server's vacuum_failsafe_age (PG14+). No concurrency test: one
// custodian per database scans in a serial loop.

// deadlineRedBuffer makes a table red from an XID age of 1e-5 of
// autovacuum_freeze_max_age (2,000 XIDs at the 200M default).
const deadlineRedBuffer = 99.999

func deadlineExec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// deadlineRedTable creates a frozen one-row table and burns XIDs until it
// is red under deadlineRedBuffer.
func deadlineRedTable(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	deadlineExec(t, pool, "DROP TABLE IF EXISTS "+table+"; CREATE TABLE "+table+
		" (id int) WITH (autovacuum_enabled = false); INSERT INTO "+table+" VALUES (1)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	deadlineExec(t, pool, "VACUUM (FREEZE) "+table)
	var maxAge int64
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('autovacuum_freeze_max_age')::bigint").Scan(&maxAge); err != nil {
		t.Fatalf("read autovacuum_freeze_max_age: %v", err)
	}
	burn := int(float64(maxAge)*(100-deadlineRedBuffer)/100) + 600
	deadlineExec(t, pool, fmt.Sprintf(`DO $$ BEGIN
		FOR i IN 1..%d LOOP PERFORM pg_catalog.pg_current_xact_id(); COMMIT; END LOOP;
		END $$`, burn))
}

// serverFailsafeAge is vacuum_failsafe_age on PG14+, 0 before.
func serverFailsafeAge(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var age int64
	err := pool.QueryRow(context.Background(), `SELECT COALESCE(
		current_setting('vacuum_failsafe_age', true)::bigint, 0)`).Scan(&age)
	if err != nil {
		t.Fatalf("read vacuum_failsafe_age: %v", err)
	}
	return age
}

func TestFreezeRedTableDeadlineIsAheadOnRealPostgres(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	table := "public.vacuum03_deadline"
	deadlineRedTable(t, pool, table)
	custodian := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	if _, err := custodian.Scan(ctx); err != nil {
		t.Fatalf("first Scan (rate sample): %v", err)
	}
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	mine := proposalsFor(proposals, table)
	if len(mine) != 1 || mine[0].Feature != "freeze" {
		t.Fatalf("proposals for %s = %#v, want one freeze", table, mine)
	}
	now := time.Now()
	deadline := mine[0].Deadline
	if deadline == nil || deadline.Kind != "xid" || !deadline.HardAt.After(now) ||
		!deadline.HardAt.Before(now.AddDate(101, 0, 0)) {
		t.Fatalf("policy deadline = %#v, want xid within (now, now+101y)", deadline)
	}
	if got := mine[0].Evidence["deadline_threat"]; got != "xid" && got != "multixact" {
		t.Fatalf("deadline_threat = %#v, want xid or multixact", got)
	}
	failsafe, ok := mine[0].Evidence["vacuum_failsafe_age"]
	if want := serverFailsafeAge(t, pool); want > 0 {
		if failsafe != want {
			t.Fatalf("vacuum_failsafe_age = %#v, want %d", failsafe, want)
		}
	} else if ok {
		t.Fatalf("vacuum_failsafe_age = %#v on a server without the setting", failsafe)
	}
}
