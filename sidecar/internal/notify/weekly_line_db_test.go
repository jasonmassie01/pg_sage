package notify

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ux-09b on real PostgreSQL: deliveries record the items they carry
// (item_refs), and the weekly line joins them to approve, reject and
// snooze decisions in sage.action_queue and to mutes. The test works in a
// week far in the future, so other tests' log rows fall outside it.

var weeklyNow = time.Date(2031, 6, 18, 12, 0, 0, 0, time.UTC)

func insertQueueItem(t *testing.T, o *outboxDB) int {
	t.Helper()
	var id int
	if err := o.pool.QueryRow(context.Background(), `INSERT INTO sage.action_queue
		(proposed_sql, action_risk) VALUES ('SELECT 1', 'safe') RETURNING id`).
		Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = o.pool.Exec(context.Background(), `DELETE FROM sage.action_queue WHERE id = $1`,
			id)
	})
	return id
}

func insertSentLog(t *testing.T, o *outboxDB, at time.Time, db string, refs []string) {
	t.Helper()
	if _, err := o.pool.Exec(context.Background(), `INSERT INTO sage.notification_log
		(channel_id, event, subject, status, sent_at, database, item_refs)
		VALUES ($1, 'approval_needed', 'weekly', 'sent', $2, NULLIF($3, ''), $4)`,
		o.ch, at, db, refs); err != nil {
		t.Fatal(err)
	}
}

func TestWeeklyLineDB_RecordDeliveryKeepsTheItems(t *testing.T) {
	o, _ := newOutboxDB(t, 200)
	ctx := context.Background()
	evt := Event{Type: "approval_needed", Severity: "warning", Subject: "items test",
		Data: map[string]any{"database": "orders", "queue_id": 41}}
	if err := o.store.RecordDelivery(ctx, Delivery{ChannelID: o.ch, Event: evt,
		Status: "sent"}); err != nil {
		t.Fatal(err)
	}
	var refs []string
	if err := o.pool.QueryRow(ctx, `SELECT item_refs FROM sage.notification_log
		WHERE channel_id = $1 AND subject = 'items test'`, o.ch).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "queue:41" {
		t.Fatalf("item_refs %v, want [queue:41]", refs)
	}
	none := Event{Type: "finding_opened", Severity: "info", Subject: "no items"}
	if err := o.store.RecordDelivery(ctx, Delivery{ChannelID: o.ch, Event: none,
		Status: "sent"}); err != nil {
		t.Fatal(err)
	}
	var null bool
	if err := o.pool.QueryRow(ctx, `SELECT item_refs IS NULL FROM sage.notification_log
		WHERE channel_id = $1 AND subject = 'no items'`, o.ch).Scan(&null); err != nil || !null {
		t.Fatalf("an event of no item stored item_refs (null=%v, %v)", null, err)
	}
}

func TestWeeklyLineDB_JoinsDecisionsAndMutes(t *testing.T) {
	o, _ := newOutboxDB(t, 200)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = o.pool.Exec(context.Background(),
			`DELETE FROM sage.notification_mutes WHERE database LIKE 'weekly_%'`)
	})
	approved, snoozed, late, ignored := insertQueueItem(t, o), insertQueueItem(t, o),
		insertQueueItem(t, o), insertQueueItem(t, o)
	t0 := weeklyNow.Add(-3 * 24 * time.Hour)
	ref := func(id int) []string { return []string{fmt.Sprintf("queue:%d", id)} }
	insertSentLog(t, o, t0, "orders", ref(approved))
	insertSentLog(t, o, t0, "orders", ref(snoozed))
	insertSentLog(t, o, t0, "orders", ref(late))
	insertSentLog(t, o, t0, "orders", ref(ignored))
	insertSentLog(t, o, t0, "weekly_muted", nil)
	insertSentLog(t, o, weeklyNow.Add(-8*24*time.Hour), "orders", nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := o.pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE sage.action_queue SET status = 'approved', decided_at = $2 WHERE id = $1`,
		approved, t0.Add(24*time.Hour))
	exec(`UPDATE sage.action_queue SET snoozed_at = $2, snoozed_until = $3 WHERE id = $1`,
		snoozed, t0.Add(time.Hour), t0.Add(5*time.Hour))
	exec(`UPDATE sage.action_queue SET status = 'rejected', decided_at = $2 WHERE id = $1`,
		late, t0.Add(24*time.Hour+time.Second))
	exec(`INSERT INTO sage.notification_mutes (database, class, until, actor, created_at)
		VALUES ('weekly_muted', '', $1, 'a', $2)`, t0.Add(48*time.Hour), t0.Add(time.Hour))
	got, err := o.store.WeeklyLine(ctx, weeklyNow)
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages != 5 || got.LedToAction != 3 ||
		got.Text != "5 messages, 3 led to an action" {
		t.Fatalf("weekly line %+v, want 5 messages and 3 led to an action", got)
	}
	empty, err := o.store.WeeklyLine(ctx, weeklyNow.Add(30*24*time.Hour))
	if err != nil || empty.Text != "No messages this week" {
		t.Fatalf("an empty week reads %+v, %v", empty, err)
	}
}

func TestWeeklyLineDB_NilPoolIsAnError(t *testing.T) {
	if _, err := NewPoolStore(nil, nil).WeeklyLine(context.Background(), weeklyNow); err == nil {
		t.Fatal("a nil pool returned a weekly line")
	}
}
