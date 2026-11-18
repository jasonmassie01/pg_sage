//go:build integration

package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ux-09b: a channel's own quiet hours are set on the channel form, stored
// in sage.notification_channels and listed with the channel; a snooze
// records when it was made, for the weekly line. No concurrent access
// test: each write is one single-row UPDATE.

func TestSetChannelQuietHours_StoresListsAndClears(t *testing.T) {
	pool, ctx := recStorePool(t)
	ns := NewNotificationStore(pool, nil)
	id := lastDeliveryChannel(t, ns, ctx, "u09b-quiet")
	if err := ns.SetChannelQuietHours(ctx, id, "22:00", "07:00",
		"America/Chicago"); err != nil {
		t.Fatalf("SetChannelQuietHours: %v", err)
	}
	ch := listingByID(t, ns, ctx)[id]
	if ch.QuietStart != "22:00" || ch.QuietEnd != "07:00" || ch.QuietTZ != "America/Chicago" {
		t.Fatalf("listed quiet hours %q-%q %q", ch.QuietStart, ch.QuietEnd, ch.QuietTZ)
	}
	if err := ns.SetChannelQuietHours(ctx, id, "", "", ""); err != nil {
		t.Fatalf("clearing quiet hours: %v", err)
	}
	if ch := listingByID(t, ns, ctx)[id]; ch.QuietStart != "" || ch.QuietTZ != "" {
		t.Fatalf("cleared quiet hours still listed: %+v", ch.Channel)
	}
}

func TestSetChannelQuietHours_RefusesBadInputAndUnknownChannel(t *testing.T) {
	pool, ctx := recStorePool(t)
	ns := NewNotificationStore(pool, nil)
	id := lastDeliveryChannel(t, ns, ctx, "u09b-quiet-bad")
	for _, c := range []struct{ start, end, tz, want string }{
		{"22:00", "22:00", "UTC", "equal"},
		{"22:00", "07:00", "Mars/Base", "Mars/Base"},
	} {
		err := ns.SetChannelQuietHours(ctx, id, c.start, c.end, c.tz)
		if err == nil || !errors.Is(err, ErrInvalidQuietHours) ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("%q-%q %q: error %v, want invalid naming %s", c.start, c.end, c.tz, err,
				c.want)
		}
	}
	if ch := listingByID(t, ns, ctx)[id]; ch.QuietStart != "" {
		t.Fatalf("a refused window was stored: %+v", ch.Channel)
	}
	err := ns.SetChannelQuietHours(ctx, 1<<30, "22:00", "07:00", "UTC")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel error %v, want ErrNotFound", err)
	}
}

func TestSnoozeRecordsWhenItWasMade(t *testing.T) {
	pool, ctx := recStorePool(t)
	id := queueForCard(t, ctx, pool, "ANALYZE public.card_u09b")
	before := time.Now().Add(-time.Second)
	until := time.Now().Add(time.Hour)
	if err := NewActionStore(pool).Snooze(ctx, id, 9, until, "later"); err != nil {
		t.Fatal(err)
	}
	var at *time.Time
	if err := pool.QueryRow(ctx, `SELECT snoozed_at FROM sage.action_queue WHERE id = $1`,
		id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at == nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Fatalf("snoozed_at = %v, want the time of the snooze", at)
	}
}
