//go:build integration

package store

import (
	"context"
	"strings"
	"testing"
)

// alerting-10 review: the channel list reads each channel's newest log row
// once per channel. With busy channels beside a quiet or never-delivered
// one, the planner prefers walking the global sent_at index and filtering
// on channel_id, which reads the whole log for the quiet channel. The
// query must take idx_notification_log_channel_sent whatever the skew.
// No concurrent access tests: this pins one read-only query's plan.

func seedSkewedLog(t *testing.T, ctx context.Context, ns *NotificationStore) {
	t.Helper()
	pool := ns.pool
	cleanup := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_log WHERE subject = 'a10-plan'`)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_channels WHERE name LIKE 'a10-plan-%'`)
		_, _ = pool.Exec(context.Background(), `ANALYZE sage.notification_log`)
	}
	cleanup()
	t.Cleanup(cleanup)
	stmts := []string{
		`INSERT INTO sage.notification_channels (name, type, config)
		 SELECT 'a10-plan-' || g, 'slack', '{}' FROM generate_series(1, 20) g`,
		// 19 busy channels interleaved minute by minute; the 20th has no row.
		`INSERT INTO sage.notification_log (channel_id, event, subject, status, sent_at)
		 SELECT c.id, 'test', 'a10-plan', 'sent', now() - make_interval(mins => g)
		   FROM sage.notification_channels c, generate_series(1, 5000) g
		  WHERE c.name LIKE 'a10-plan-%' AND c.name <> 'a10-plan-20'`,
		`ANALYZE sage.notification_log`,
		`ANALYZE sage.notification_channels`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seeding the skewed log: %v\n%s", err, s)
		}
	}
}

func TestListChannels_PlanUsesThePerChannelIndexUnderSkew(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	seedSkewedLog(t, ctx, ns)

	rows, err := pool.Query(ctx, "EXPLAIN "+listChannelsSQL)
	if err != nil {
		t.Fatalf("EXPLAIN listChannelsSQL: %v", err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scanning plan: %v", err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading plan: %v", err)
	}
	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "idx_notification_log_channel_sent") ||
		strings.Contains(text, "idx_notification_log_sent_at") {
		t.Errorf("the channel list must probe idx_notification_log_channel_sent, "+
			"not filter the global sent_at index; plan:\n%s", text)
	}

	got := listingByID(t, ns, ctx)
	busy, quiet := 0, 0
	for _, ch := range got {
		if !strings.HasPrefix(ch.Name, "a10-plan-") {
			continue
		}
		if ch.Name == "a10-plan-20" {
			quiet++
			if ch.LastSentAt != nil || ch.LastStatus != "" {
				t.Errorf("never-delivered channel shows %q at %v", ch.LastStatus, ch.LastSentAt)
			}
			continue
		}
		busy++
		if ch.LastStatus != "sent" || ch.LastSentAt == nil {
			t.Errorf("busy channel %s shows %q at %v, want sent", ch.Name,
				ch.LastStatus, ch.LastSentAt)
		}
	}
	if busy != 19 || quiet != 1 {
		t.Errorf("listed %d busy and %d quiet channels, want 19 and 1", busy, quiet)
	}
}
