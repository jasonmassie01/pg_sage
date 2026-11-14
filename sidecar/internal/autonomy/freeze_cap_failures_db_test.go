package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-05b on real PostgreSQL: readVacuumCapFailures counts the table's
// custodian VACUUMs the watchdog capped in the last 6 hours, and nothing
// else. No concurrent access test: the custodian reads it once per red
// table on its one scan goroutine.

// capRow is one seeded sage.action_log row.
type capRow struct {
	sql, outcome, reason string
	age                  time.Duration
}

func capReason(outcome string) string {
	return outcome + ": the watchdog cancelled VACUUM backend 4242 (scanning heap 3/500)"
}

func seedCapRows(t *testing.T, pool *pgxpool.Pool, rows ...capRow) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rows {
		sql := r.sql
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				"DELETE FROM sage.action_log WHERE sql_executed = $1", sql)
		})
		var reason *string
		if r.reason != "" {
			reason = &r.reason
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log
			(action_type, sql_executed, outcome, rollback_reason, executed_at)
			VALUES ('vacuum', $1, $2, $3, now() - make_interval(secs => $4))`,
			r.sql, r.outcome, reason, r.age.Seconds()); err != nil {
			t.Fatalf("seed action_log row %+v: %v", r, err)
		}
	}
}

func uniqueCapTable() string { return fmt.Sprintf("capfail_%d", time.Now().UnixNano()) }

func readCaps(t *testing.T, c *PostgresFreezeCustodian, table string) int {
	t.Helper()
	n, err := c.readVacuumCapFailures(context.Background(), "public", table)
	if err != nil {
		t.Fatalf("readVacuumCapFailures(%s): %v", table, err)
	}
	return n
}

// Happy path: capped runs of every custodian form of the table count,
// failed and rollback_failed alike.
func TestReadVacuumCapFailuresCountsEveryCustodianForm(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := uniqueCapTable()
	forms := freeze.CustodianVacuumStatements("public", table)
	rows := make([]capRow, 0, len(forms))
	for i, sql := range forms {
		outcome := "failed"
		if i == 1 {
			outcome = "rollback_failed"
		}
		rows = append(rows, capRow{sql, outcome, capReason("vacuum_cap"), time.Minute})
	}
	seedCapRows(t, pool, rows...)
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	if got := readCaps(t, c, table); got != len(forms) {
		t.Fatalf("cap failures = %d, want %d (one per form)", got, len(forms))
	}
}

// Invalid input: a stalled run, a lock timeout, a reason without the
// prefix, a success, another table and another statement never count.
func TestReadVacuumCapFailuresIgnoresOtherFailures(t *testing.T) {
	pool := requireAutonomyDB(t)
	table, other := uniqueCapTable(), uniqueCapTable()+"_other"
	sql := freeze.VacuumFreezeSQL("public", table)
	seedCapRows(t, pool,
		capRow{sql, "failed", capReason("vacuum_stalled"), time.Minute},
		capRow{sql, "failed", "lock_timeout: canceling statement due to lock timeout",
			time.Minute},
		capRow{sql, "failed", "execute: vacuum_cap reached", time.Minute},
		capRow{sql, "failed", "", time.Minute},
		capRow{freeze.VacuumFreezeSQL("public", other), "failed", capReason("vacuum_cap"),
			time.Minute},
		capRow{`ANALYZE "public"."` + table + `"`, "failed", capReason("vacuum_cap"),
			time.Minute},
		capRow{sql, "monitoring", capReason("vacuum_cap"), 2 * time.Hour},
	)
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	if got := readCaps(t, c, table); got != 0 {
		t.Fatalf("cap failures = %d, want 0: none of the rows is a capped custodian VACUUM",
			got)
	}
}

// Boundary: a cap 6 h + 1 s old has aged out; one just inside counts.
func TestReadVacuumCapFailuresWindowEdge(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := uniqueCapTable()
	sql := freeze.VacuumFreezeSQL("public", table)
	seedCapRows(t, pool,
		capRow{sql, "failed", capReason("vacuum_cap"), 6*time.Hour + time.Second},
		capRow{sql, "failed", capReason("vacuum_cap"), 6*time.Hour - time.Minute})
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	if got := readCaps(t, c, table); got != 1 {
		t.Fatalf("cap failures = %d, want 1 (only the one inside the 6 h window)", got)
	}
}

// State transition: a completed run ends the streak; caps after it count.
func TestReadVacuumCapFailuresCountsOnlyCapsAfterTheLastSuccess(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := uniqueCapTable()
	plain := freeze.VacuumFreezeSQL("public", table)
	emergency := freeze.EmergencyVacuumSQL("public", table, 0, 0, freeze.AggressiveAges{})
	seedCapRows(t, pool,
		capRow{plain, "failed", capReason("vacuum_cap"), 3 * time.Hour},
		capRow{plain, "failed", capReason("vacuum_cap"), 2 * time.Hour},
		capRow{emergency, "success", "", time.Hour},
		capRow{plain, "failed", capReason("vacuum_cap"), 30 * time.Minute})
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	if got := readCaps(t, c, table); got != 1 {
		t.Fatalf("cap failures = %d, want 1 (the success ended the earlier streak)", got)
	}
}

// Nil/empty: no rows reads zero.
func TestReadVacuumCapFailuresWithoutHistoryIsZero(t *testing.T) {
	pool := requireAutonomyDB(t)
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	if got := readCaps(t, c, uniqueCapTable()); got != 0 {
		t.Fatalf("cap failures without history = %d, want 0", got)
	}
}

// Error propagation: a cancelled read and a missing pool say what failed.
func TestReadVacuumCapFailuresErrors(t *testing.T) {
	pool := requireAutonomyDB(t)
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := c.readVacuumCapFailures(ctx, "public", "orders")
	if err == nil || n != 0 || !strings.Contains(err.Error(), "read vacuum_cap failures") {
		t.Fatalf("cancelled read = %d, %v; want 0 and a named error", n, err)
	}
	none := NewPostgresFreezeCustodian(nil, "testdb", deadlineRedBuffer)
	n, err = none.readVacuumCapFailures(context.Background(), "public", "orders")
	if err == nil || n != 0 || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("read without a pool = %d, %v; want 0 and a pool error", n, err)
	}
}
