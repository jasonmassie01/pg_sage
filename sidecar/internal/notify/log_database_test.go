package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-06c: each sage.notification_log row names the database and the
// dedup key of the event it delivered, so a fleet operator can tell which
// database a delivery was for. A fleet-wide event writes NULL, never
// "all". A failed log insert never fails the delivery.
//
// No concurrent access tests: the writer is per delivery and both columns
// are filled in the same INSERT as the rest of the row.
// No state transition tests: the log is append-only.

type logDBRow struct {
	database, dedupKey *string
	event, status      string
}

func notifyLogDB(t *testing.T) (*pgxpool.Pool, *Dispatcher, *mockSender, int) {
	t.Helper()
	pool := connectTestDB(t)
	tag := fmt.Sprintf("a06c_test_%d", time.Now().UnixNano())
	chID := insertTestChannel(t, pool, tag, "slack",
		map[string]string{"webhook_url": "https://hooks.example.test/x"}, true)
	insertTestRule(t, pool, chID, "finding_opened", "info", true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM sage.notification_log WHERE channel_id = $1`, chID)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.notification_rules WHERE channel_id = $1`, chID)
		_, _ = pool.Exec(ctx, `DELETE FROM sage.notification_channels WHERE id = $1`, chID)
	})
	d := NewDispatcher(pool, func(string, string, ...any) {})
	sender := newMockSender("slack")
	d.RegisterSender(sender)
	return pool, d, sender, chID
}

func readLogRows(t *testing.T, pool *pgxpool.Pool, chID int) []logDBRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT database, dedup_key, event, status
		FROM sage.notification_log WHERE channel_id = $1 ORDER BY id`, chID)
	if err != nil {
		t.Fatalf("read notification_log: %v", err)
	}
	defer rows.Close()
	var out []logDBRow
	for rows.Next() {
		var r logDBRow
		if err := rows.Scan(&r.database, &r.dedupKey, &r.event, &r.status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func strOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Happy path: a delivery for db2 in fleet mode records db2 and the
// event's dedup key.
func TestLogDatabase_DeliveryRecordsDatabaseAndDedupKey(t *testing.T) {
	pool, d, sender, chID := notifyLogDB(t)
	evt := Event{Type: "finding_opened", Severity: "warning", Subject: "a06c bloat",
		DedupKey: "finding:42", Data: map[string]any{"database": "db2"}}
	res, err := d.Submit(context.Background(), evt)
	if err != nil || res.Delivered != 1 || sender.callCount() != 1 {
		t.Fatalf("submit = %+v, %v; sends %d; want one delivery", res, err, sender.callCount())
	}
	rows := readLogRows(t, pool, chID)
	if len(rows) != 1 {
		t.Fatalf("%d log rows, want 1", len(rows))
	}
	r := rows[0]
	if strOrNil(r.database) != "db2" || strOrNil(r.dedupKey) != "finding:42" ||
		r.status != "sent" || r.event != "finding_opened" {
		t.Fatalf("row = database %s dedup_key %s %s/%s, want db2 finding:42 finding_opened/sent",
			strOrNil(r.database), strOrNil(r.dedupKey), r.event, r.status)
	}
}

// An event with no explicit dedup key records the key the channels
// deduplicate on (EventDedupKey), never an empty string.
func TestLogDatabase_DerivedDedupKeyIsRecorded(t *testing.T) {
	pool, d, _, chID := notifyLogDB(t)
	evt := Event{Type: "finding_opened", Severity: "warning", Subject: "a06c derived",
		Data: map[string]any{"database": "db1"}}
	if _, err := d.Submit(context.Background(), evt); err != nil {
		t.Fatalf("submit: %v", err)
	}
	rows := readLogRows(t, pool, chID)
	if len(rows) != 1 || strOrNil(rows[0].dedupKey) != EventDedupKey(evt) {
		t.Fatalf("rows = %+v, want dedup_key %q", rows, EventDedupKey(evt))
	}
	if !strings.HasPrefix(EventDedupKey(evt), "finding_opened:db1:") {
		t.Fatalf("derived key %q does not name the event and database", EventDedupKey(evt))
	}
}

// Nil/empty: a fleet-wide event (no database, an empty one, or the "all"
// selector) writes NULL, never "all" or "".
func TestLogDatabase_FleetWideEventWritesNull(t *testing.T) {
	pool, d, _, chID := notifyLogDB(t)
	for i, data := range []map[string]any{nil, {}, {"database": ""},
		{"database": "all"}, {"database": nil}} {
		evt := Event{Type: "finding_opened", Severity: "warning",
			Subject: fmt.Sprintf("a06c broadcast %d", i), Data: data}
		if _, err := d.Submit(context.Background(), evt); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	rows := readLogRows(t, pool, chID)
	if len(rows) != 5 {
		t.Fatalf("%d log rows, want 5", len(rows))
	}
	for i, r := range rows {
		if r.database != nil {
			t.Errorf("row %d database = %q, want NULL", i, *r.database)
		}
		if r.dedupKey == nil || *r.dedupKey == "" {
			t.Errorf("row %d dedup_key = %s, want the derived key", i, strOrNil(r.dedupKey))
		}
	}
}

// failingRecorder is a MemoryStore whose delivery log insert fails.
type failingRecorder struct {
	*MemoryStore
}

func (f failingRecorder) RecordDelivery(context.Context, Delivery) error {
	return errors.New("insert notification_log: connection refused")
}

func (f failingRecorder) LogDelivery(context.Context, int, Event, string, string) error {
	return errors.New("insert notification_log: connection refused")
}

type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logFn(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logCapture) errors() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		if strings.HasPrefix(line, "ERROR ") {
			out = append(out, line)
		}
	}
	return out
}

func failingLogDispatcher(t *testing.T) (*Dispatcher, *mockSender, *logCapture) {
	t.Helper()
	mem := NewMemoryStore()
	chID := mem.AddChannel(Channel{ID: 7, Name: "ops", Type: "slack", Enabled: true})
	mem.AddRule(Rule{ChannelID: chID, Event: "finding_opened", MinSeverity: "info",
		Enabled: true})
	logs := &logCapture{}
	d := NewDispatcherWithStore(failingRecorder{mem}, logs.logFn)
	sender := newMockSender("slack")
	d.RegisterSender(sender)
	return d, sender, logs
}

// Error propagation: a log insert failure is logged with the channel and
// the event, and the delivery still counts as delivered.
func TestLogDatabase_LogInsertFailureDoesNotFailDelivery(t *testing.T) {
	d, sender, logs := failingLogDispatcher(t)
	res, err := d.Submit(context.Background(), Event{Type: "finding_opened",
		Severity: "warning", Subject: "a06c", Data: map[string]any{"database": "db2"}})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Delivered != 1 || res.Failed != 0 || res.Refused != 0 || sender.callCount() != 1 {
		t.Fatalf("result = %+v, sends %d; want 1 delivered, 0 failed", res, sender.callCount())
	}
	errs := logs.errors()
	if len(errs) != 1 {
		t.Fatalf("error lines = %q, want exactly one", errs)
	}
	for _, want := range []string{"channel 7", "finding_opened", "db2", "connection refused"} {
		if !strings.Contains(errs[0], want) {
			t.Errorf("error line %q does not name %q", errs[0], want)
		}
	}
}

// Boundary: a send failure is still a failed delivery when the log insert
// fails too; the insert failure never masks it.
func TestLogDatabase_SendFailureStillCountsWhenLogFails(t *testing.T) {
	d, sender, logs := failingLogDispatcher(t)
	sender.failNext = true
	res, err := d.Submit(context.Background(), Event{Type: "finding_opened",
		Severity: "warning", Subject: "a06c fail"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Delivered != 0 || res.Failed != 1 {
		t.Fatalf("result = %+v, want 1 failed", res)
	}
	errs := strings.Join(logs.errors(), "\n")
	if !strings.Contains(errs, "channel 7") || !strings.Contains(errs, "send failed") {
		t.Fatalf("error lines %q lack the log failure or the send failure", errs)
	}
}
