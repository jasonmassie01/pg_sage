package notify

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// alerting-06 review on real PostgreSQL: open info alerts (info never
// reminds) delivered long ago must not fill the bounded reminder scan of
// sage.notification_state and starve a warning whose reminder is due.

func TestOutboxDB_NeverRemindingAlertsDoNotStarveDueReminders(t *testing.T) {
	o, seq := newOutboxDB(t, 200)
	d := o.dispatcher(t)
	ctx := context.Background()
	old := o.clock.Now().Add(-72 * time.Hour)
	for i := 0; i < reminderScanLimit+20; i++ {
		if _, err := o.pool.Exec(ctx, `INSERT INTO sage.notification_state
			(database, dedup_key, channel_id, open, severity, first_sent_at, last_sent_at,
			 payload)
			VALUES ('db1', $1, $2, true, 'info', $3, $3, '{"type":"finding_opened"}')`,
			fmt.Sprintf("finding:db1:info:%d", i), o.ch, old); err != nil {
			t.Fatalf("insert open info state: %v", err)
		}
	}
	if _, err := d.Submit(ctx, findingEvt("warning")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	o.clock.Advance(60 * time.Minute)
	got, err := d.DrainOutbox(ctx)
	if err != nil || got.Reminders != 1 {
		t.Fatalf("drain at the interval = %+v, %v; want the warning's reminder", got, err)
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if _, accepted := seq.counts(); accepted != 2 {
		t.Fatalf("accepted = %d, want the alert and its reminder only", accepted)
	}
}
