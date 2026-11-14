//go:build integration

package logwatch

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-07b on real PostgreSQL: a deadlock between two sessions whose
// statements carry literals becomes an incident whose log_occurrences
// name both participants, their lock mode and two fingerprints, with no
// literal anywhere in sage.incidents.

var participantLiterals = []string{"alice@example.com", "4111111111111111",
	"bob@example.org", "5500000000000004"}

// literalDeadlock is a real deadlock and the server-log DETAIL PostgreSQL
// writes for it: the client DETAIL (wait lines) plus one "Process N:
// <statement>" line per process (errdetail_log, never sent to the client).
type literalDeadlock struct {
	deadlockResult
	logDetail, context string
}

func createLiteralTable(ctx context.Context, t *testing.T, c *pgx.Conn) {
	t.Helper()
	if _, err := c.Exec(ctx, "CREATE TABLE IF NOT EXISTS rca07b_cards "+
		"(id int PRIMARY KEY, email text, card bigint);"+
		"INSERT INTO rca07b_cards VALUES (1, '', 0), (2, '', 0) "+
		"ON CONFLICT DO NOTHING"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "DROP TABLE IF EXISTS rca07b_cards")
	})
}

func provokeLiteralDeadlock(ctx context.Context, t *testing.T, url string) literalDeadlock {
	t.Helper()
	a, pidA := connectWithPID(ctx, t, url)
	b, pidB := connectWithPID(ctx, t, url)
	createLiteralTable(ctx, t, a)
	stmt := func(email, card string, id int) string {
		return fmt.Sprintf("UPDATE rca07b_cards SET email = '%s' WHERE id = %d AND card <> %s",
			email, id, card)
	}
	// The two colliding statements have different shapes: one statement
	// shape has one fingerprint whatever its values.
	sqlA2 := stmt("alice@example.com", "4111111111111111", 2)
	sqlB2 := fmt.Sprintf("UPDATE rca07b_cards SET card = %s WHERE id = %d AND email <> '%s'",
		"5500000000000004", 1, "bob@example.org")
	for _, step := range []struct {
		c   *pgx.Conn
		sql string
	}{{a, stmt("x", "1", 1)}, {b, stmt("y", "2", 2)}} {
		if _, err := step.c.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := step.c.Exec(ctx, step.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("first update: %v", err)
		}
	}
	errA := make(chan error, 1)
	go func() { _, err := a.Exec(ctx, sqlA2, pgx.QueryExecModeSimpleProtocol); errA <- err }()
	time.Sleep(200 * time.Millisecond)
	_, errB := b.Exec(ctx, sqlB2, pgx.QueryExecModeSimpleProtocol)
	gotA := <-errA
	for _, c := range []*pgx.Conn{a, b} {
		_, _ = c.Exec(ctx, "ROLLBACK")
	}
	var pgErr *pgconn.PgError
	var d deadlockResult
	switch {
	case errors.As(errB, &pgErr):
		d = deadlockResult{victim: pidB, other: pidA, err: pgErr}
	case errors.As(gotA, &pgErr):
		d = deadlockResult{victim: pidA, other: pidB, err: pgErr}
	default:
		t.Fatalf("no deadlock error: a=%v b=%v", gotA, errB)
	}
	logDetail := d.err.Detail + fmt.Sprintf("\nProcess %d: %s\nProcess %d: %s",
		pidA, sqlA2, pidB, sqlB2)
	return literalDeadlock{deadlockResult: d, logDetail: logDetail, context: d.err.Where}
}

// writeContextCSVRecord writes the victim's server-log record, with the
// CONTEXT in column 18 as PostgreSQL's csvlog does.
func writeContextCSVRecord(t *testing.T, dir, datname string, d literalDeadlock) {
	t.Helper()
	rec := make([]string, csvExtColumns)
	rec[csvColTime] = time.Now().UTC().Format("2006-01-02 15:04:05.000") + " UTC"
	rec[csvColUser], rec[csvColDatabase] = "postgres", datname
	rec[csvColPID] = strconv.Itoa(d.victim)
	rec[csvColSeverity], rec[csvColSQLState] = "ERROR", d.err.Code
	rec[csvColMessage], rec[csvColDetail] = d.err.Message, d.logDetail
	rec[csvColContext] = d.context
	rec[csvColApplication], rec[csvColBackendType] = "rca07b_it", "client backend"
	f, err := os.Create(filepath.Join(dir, "postgresql.csv"))
	if err != nil {
		t.Fatal(err)
	}
	w := csv.NewWriter(f)
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if err := errors.Join(w.Error(), f.Close()); err != nil {
		t.Fatal(err)
	}
}

// CHECK-22 (integration): the stored incident names both PIDs, the lock
// mode and two fingerprints, and holds no literal of the statements.
func TestIntegration_DeadlockParticipantsStoredWithoutLiterals(t *testing.T) {
	url := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := bootstrapPool(ctx, t, url)
	var datname string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&datname); err != nil {
		t.Fatal(err)
	}
	d := provokeLiteralDeadlock(ctx, t, url)
	if d.err.Code != "40P01" || !strings.Contains(d.context, "rca07b_cards") {
		t.Fatalf("victim error = %s %s (context %q), want 40P01 on rca07b_cards",
			d.err.Code, d.err.Message, d.context)
	}
	dir := t.TempDir()
	writeContextCSVRecord(t, dir, datname, d)
	name := fmt.Sprintf("rca07b_it_%d", time.Now().UnixNano())
	eng := newCrashEngine()
	eng.WithDatabaseName(name)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", name)
	})
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 100, DedupWindowS: 60, MaxLinesPerCycle: 1000}, nopLog)
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer fw.Stop()
	eng.SetLogSource(fw)
	cfg := &config.Config{RCA: config.RCAConfig{Enabled: true}}
	eng.AnalyzeContext(ctx, &collector.Snapshot{CollectedAt: time.Now()}, nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	assertStoredParticipants(ctx, t, pool, name, d)
}

func assertStoredParticipants(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, d literalDeadlock,
) {
	t.Helper()
	var all, occRaw string
	err := pool.QueryRow(ctx, `SELECT row_to_json(i)::text, i.log_occurrences::text
		FROM sage.incidents i WHERE database_name = $1
		AND 'log_deadlock_detected' = ANY(signal_ids)`, name).Scan(&all, &occRaw)
	if err != nil {
		t.Fatalf("stored deadlock incident: %v", err)
	}
	var occ rca.LogOccurrences
	if err := json.Unmarshal([]byte(occRaw), &occ); err != nil {
		t.Fatalf("decode %s: %v", occRaw, err)
	}
	byPID := map[int]rca.DeadlockParticipant{}
	for _, p := range occ.Participants {
		byPID[p.PID] = p
	}
	victim, other := byPID[d.victim], byPID[d.other]
	if victim.BlockedBy != d.other || other.BlockedBy != d.victim ||
		victim.WaitMode != "ShareLock" || other.WaitMode != "ShareLock" {
		t.Errorf("participants %s do not name the cycle %d <-> %d on ShareLock",
			occRaw, d.victim, d.other)
	}
	if victim.Fingerprint == "" || other.Fingerprint == "" ||
		victim.Fingerprint == other.Fingerprint {
		t.Errorf("fingerprints %q, %q; want two distinct", victim.Fingerprint, other.Fingerprint)
	}
	if victim.Relation != "rca07b_cards" {
		t.Errorf("victim relation = %q, want rca07b_cards from CONTEXT", victim.Relation)
	}
	for _, leak := range append(participantLiterals, `"statement"`) {
		if strings.Contains(all, leak) {
			t.Errorf("sage.incidents row holds %q: %s", leak, all)
		}
	}
}

// The same from the server's own csvlog, when a csvlog server with a
// locally mounted log directory is available.
func TestIntegration_ServerCSVLogDeadlockParticipants(t *testing.T) {
	url, _, dir := crashServerEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 200, DedupWindowS: 1}, nopLog)
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("start watcher on %s: %v", dir, err)
	}
	defer fw.Stop()
	discardBacklog(fw)
	d := provokeLiteralDeadlock(ctx, t, url)
	for ctx.Err() == nil {
		for _, s := range fw.Drain() {
			parts, _ := s.Metrics[rca.MetricDeadlockParticipants].([]rca.DeadlockParticipant)
			if s.ID == "log_deadlock_detected" && len(parts) == 2 &&
				parts[0].Fingerprint != "" && parts[1].Fingerprint != "" {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no deadlock signal with two fingerprinted participants (pids %d, %d)",
		d.victim, d.other)
}
