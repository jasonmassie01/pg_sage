//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-06c: the notification log reader lists the database and dedup
// key of each delivery and filters by database. Rows written before the
// column existed (NULL) read "unknown", are listed without a filter and
// are excluded by one.
//
// No concurrent access tests: ListLogFor is one read-only SELECT.
// No state transition tests: the log is append-only.

type logFixture struct {
	db2, db1, subject string
}

// seedLog writes, oldest first, a pre-step row (NULL database), a db1 row
// and a db2 row, all under one unique subject.
func seedLog(t *testing.T, pool *pgxpool.Pool) logFixture {
	t.Helper()
	ctx := context.Background()
	n := time.Now().UnixNano()
	f := logFixture{db2: fmt.Sprintf("a06c_db2_%d", n), db1: fmt.Sprintf("a06c_db1_%d", n),
		subject: fmt.Sprintf("a06c_log_%d", n)}
	_, err := pool.Exec(ctx, `INSERT INTO sage.notification_log
		(event, subject, status, database, dedup_key, sent_at) VALUES
		('finding_opened', $1, 'sent', NULL, NULL, now() - interval '3 minutes'),
		('finding_opened', $1, 'sent', $2, 'finding:1', now() - interval '2 minutes'),
		('incident_detected', $1, 'error', $3, 'incident:9', now() - interval '1 minute')`,
		f.subject, f.db1, f.db2)
	if err != nil {
		t.Fatalf("seed notification_log: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_log WHERE subject = $1`, f.subject)
	})
	return f
}

func ofSubject(entries []NotificationLogEntry, subject string) []NotificationLogEntry {
	var out []NotificationLogEntry
	for _, e := range entries {
		if e.Subject == subject {
			out = append(out, e)
		}
	}
	return out
}

// Happy path and boundary: no filter lists all three rows newest first,
// the pre-step row reads "unknown" (never "all").
func TestListLogFor_NoFilterListsEveryRowWithDatabase(t *testing.T) {
	pool, ctx := requireDB(t)
	f := seedLog(t, pool)
	ns := NewNotificationStore(pool, nil)
	all, err := ns.ListLogFor(ctx, "", 500)
	if err != nil {
		t.Fatalf("ListLogFor: %v", err)
	}
	got := ofSubject(all, f.subject)
	if len(got) != 3 {
		t.Fatalf("%d seeded rows listed, want 3", len(got))
	}
	want := []struct{ db, key, event string }{{f.db2, "incident:9", "incident_detected"},
		{f.db1, "finding:1", "finding_opened"}, {"unknown", "", "finding_opened"}}
	for i, w := range want {
		if got[i].Database != w.db || got[i].DedupKey != w.key || got[i].Event != w.event {
			t.Errorf("row %d = %s/%s/%s, want %s/%s/%s", i, got[i].Database,
				got[i].DedupKey, got[i].Event, w.db, w.key, w.event)
		}
	}
}

// Boundary: a database filter keeps only that database's rows and drops
// the NULL rows written before the step.
func TestListLogFor_DatabaseFilterExcludesOthersAndNull(t *testing.T) {
	pool, ctx := requireDB(t)
	f := seedLog(t, pool)
	ns := NewNotificationStore(pool, nil)
	got, err := ns.ListLogFor(ctx, f.db2, 500)
	if err != nil {
		t.Fatalf("ListLogFor(db2): %v", err)
	}
	if len(got) != 1 || got[0].Database != f.db2 || got[0].Status != "error" ||
		got[0].Subject != f.subject {
		t.Fatalf("db2 rows = %+v, want exactly the one db2 row", got)
	}
}

// Nil/empty: a database with no deliveries is an empty list, not an error.
func TestListLogFor_DatabaseWithNoRowsIsEmpty(t *testing.T) {
	pool, ctx := requireDB(t)
	seedLog(t, pool)
	ns := NewNotificationStore(pool, nil)
	got, err := ns.ListLogFor(ctx, fmt.Sprintf("a06c_none_%d", time.Now().UnixNano()), 500)
	if err != nil || len(got) != 0 {
		t.Fatalf("rows = %+v, err = %v; want none and no error", got, err)
	}
}

// Must not change: the existing limit still bounds the filtered read. db1
// gets a second, newer row, so limit 1 must drop the older one.
func TestListLogFor_LimitStillApplies(t *testing.T) {
	pool, ctx := requireDB(t)
	f := seedLog(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.notification_log
		(event, subject, status, database, dedup_key)
		VALUES ('finding_resolved', $1, 'sent', $2, 'finding:2')`,
		f.subject, f.db1); err != nil {
		t.Fatalf("seed a newer db1 row: %v", err)
	}
	ns := NewNotificationStore(pool, nil)
	got, err := ns.ListLogFor(ctx, f.db1, 1)
	if err != nil || len(got) != 1 || got[0].Database != f.db1 ||
		got[0].DedupKey != "finding:2" {
		t.Fatalf("limit 1 rows = %+v, err = %v; want only the newest db1 row", got, err)
	}
	both, err := ns.ListLogFor(ctx, f.db1, 500)
	if err != nil || len(both) != 2 {
		t.Fatalf("db1 rows = %+v, err = %v; want both db1 rows", both, err)
	}
}

// Error propagation: a canceled context is an error naming the read.
func TestListLogFor_CanceledContextIsAnError(t *testing.T) {
	pool, _ := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ns.ListLogFor(ctx, "db2", 10); err == nil {
		t.Fatal("ListLogFor on a canceled context succeeded")
	}
}
