package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-05b on real PostgreSQL: a red table whose custodian VACUUM was
// capped twice is proposed with the emergency profile; once that freeze
// succeeds and the table leaves red, its next proposal is the normal one.
// No concurrency test: one custodian per database scans in a serial loop.

func emergencyTable(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	bare := fmt.Sprintf("vacuum05b_%d", time.Now().UnixNano())
	deadlineRedTable(t, pool, "public."+bare)
	return bare, "public." + bare
}

func burnXIDs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
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

func scanFreezeProposal(t *testing.T, c *PostgresFreezeCustodian, table string) *Proposal {
	t.Helper()
	proposals, err := c.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	mine := proposalsFor(proposals, table)
	if len(mine) == 0 {
		return nil
	}
	if len(mine) != 1 || mine[0].Feature != "freeze" {
		t.Fatalf("proposals for %s = %#v, want at most one freeze", table, mine)
	}
	return &mine[0]
}

// warnLog captures the custodian's log lines.
type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) log(level, format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, level+" "+fmt.Sprintf(format, args...))
}

func (w *warnLog) warnings() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, line := range w.lines {
		if strings.HasPrefix(line, "WARN ") {
			out = append(out, line)
		}
	}
	return out
}

// Happy path and state transitions: normal, two caps, emergency, the
// emergency freeze completes and the table leaves red, then normal again.
// On the exclusive server (ci-23): another package's open transaction on
// the shared server holds the xmin horizon, and the freeze cannot advance
// relfrozenxid past it.
func TestFreezeEmergencyProfileLifecycleOnRealPostgres(t *testing.T) {
	pool := exclusiveAutonomyDB(t, "the xmin horizon an emergency freeze advances past")
	bare, table := emergencyTable(t, pool)
	logs := &warnLog{}
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer).WithLogger(logs.log)
	normal := freeze.VacuumFreezeSQL("public", bare)
	first := scanFreezeProposal(t, c, table)
	if first == nil || first.SQL != normal || first.Evidence["profile"] != nil {
		t.Fatalf("first proposal = %#v, want the normal %q", first, normal)
	}
	seedCapRows(t, pool, capRow{normal, "failed", capReason("vacuum_cap"), 20 * time.Minute},
		capRow{normal, "failed", capReason("vacuum_cap"), 10 * time.Minute})
	emergency := freeze.EmergencyVacuumSQL("public", bare, 0, 0, freeze.AggressiveAges{})
	second := scanFreezeProposal(t, c, table)
	if second == nil || second.SQL != emergency ||
		!strings.Contains(second.SQL, "INDEX_CLEANUP OFF") ||
		!strings.Contains(second.SQL, "TRUNCATE OFF") ||
		second.Evidence["profile"] != freeze.ProfileEmergency ||
		second.Evidence["cap_failures"] != 2 {
		t.Fatalf("second proposal = %#v, want the emergency %q", second, emergency)
	}
	deadlineExec(t, pool, second.SQL)
	seedCapRows(t, pool, capRow{emergency, "success", "", 0})
	var age int64
	if err := pool.QueryRow(context.Background(), `SELECT age(relfrozenxid)::bigint
		FROM pg_class WHERE oid = to_regclass($1)`, table).Scan(&age); err != nil {
		t.Fatalf("read age of %s: %v", table, err)
	}
	if third := scanFreezeProposal(t, c, table); third != nil {
		t.Fatalf("after the emergency freeze (age %d) proposal = %#v, want none", age, third)
	}
	burnXIDs(t, pool)
	fourth := scanFreezeProposal(t, c, table)
	if fourth == nil || fourth.SQL != normal || fourth.Evidence["profile"] != nil {
		t.Fatalf("red again after a success: proposal = %#v, want the normal %q",
			fourth, normal)
	}
	if w := logs.warnings(); len(w) != 0 {
		t.Fatalf("warnings = %q, want none", w)
	}
}

// Error propagation: an unreadable cap history keeps the normal SQL and
// logs exactly one WARN naming the table; the freeze is never blocked.
func TestFreezeCapFailuresReadErrorKeepsNormalSQLAndWarnsOnce(t *testing.T) {
	pool := requireAutonomyDB(t)
	bare, table := emergencyTable(t, pool)
	logs := &warnLog{}
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer).WithLogger(logs.log)
	reads := 0
	c.capFailures = func(_ context.Context, schema, name string) (int, error) {
		if schema == "public" && name == bare {
			reads++
		}
		return 5, errors.New("read vacuum_cap failures: connection refused")
	}
	got := scanFreezeProposal(t, c, table)
	if want := freeze.VacuumFreezeSQL("public", bare); got == nil || got.SQL != want ||
		got.Evidence["profile"] != nil {
		t.Fatalf("proposal = %#v, want the normal %q", got, want)
	}
	if reads != 1 {
		t.Fatalf("cap history reads for %s = %d, want 1", table, reads)
	}
	var mine []string
	for _, line := range logs.warnings() {
		if strings.Contains(line, table) {
			mine = append(mine, line)
		}
	}
	if len(mine) != 1 || !strings.Contains(mine[0], "connection refused") {
		t.Fatalf("warnings for %s = %q, want exactly one with the cause", table, mine)
	}
}

// Invalid input: a stalled run and a lock timeout next to one cap keep the
// normal SQL (only vacuum_cap counts).
func TestFreezeStalledAndLockTimeoutDoNotTriggerEmergency(t *testing.T) {
	pool := requireAutonomyDB(t)
	bare, table := emergencyTable(t, pool)
	normal := freeze.VacuumFreezeSQL("public", bare)
	seedCapRows(t, pool,
		capRow{normal, "failed", capReason("vacuum_cap"), 30 * time.Minute},
		capRow{normal, "failed", capReason("vacuum_stalled"), 20 * time.Minute},
		capRow{normal, "failed", "lock_timeout: canceling statement due to lock timeout",
			10 * time.Minute})
	c := NewPostgresFreezeCustodian(pool, "testdb", deadlineRedBuffer)
	got := scanFreezeProposal(t, c, table)
	if got == nil || got.SQL != normal || got.Evidence["profile"] != nil {
		t.Fatalf("proposal = %#v, want the normal %q", got, normal)
	}
}

// Boundary: amber tables never read the cap history and never go emergency.
func TestFreezeAmberTableIgnoresCapFailures(t *testing.T) {
	pool := requireAutonomyDB(t)
	table := "public.vacuum05b_amber"
	amberTunedTable(t, table)
	c := NewPostgresFreezeCustodian(pool, "testdb", amberRedBuffer)
	requireAmber(t, table, c.threshold)
	reads := 0
	c.capFailures = func(context.Context, string, string) (int, error) {
		reads++
		return 5, nil
	}
	if got := scanFreezeProposal(t, c, table); got != nil {
		t.Fatalf("amber proposal = %#v, want none", got)
	}
	if reads != 0 {
		t.Fatalf("cap history reads for an amber scan = %d, want 0", reads)
	}
}
