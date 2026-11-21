package notify

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// ux-09b on real PostgreSQL: sage.notification_mutes and the channel quiet
// hours columns.

func muteDBName(t *testing.T) string {
	t.Helper()
	db := fmt.Sprintf("mute_%d", time.Now().UnixNano())
	o := connectTestDB(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = o.Exec(ctx, `DELETE FROM sage.notification_mutes WHERE database = $1`, db)
	})
	return db
}

// Concurrency: two admins mute the same database at once. One row
// results and the later until wins.
func TestMuteDB_ConcurrentAdminsKeepOneRowAndTheLaterUntil(t *testing.T) {
	pool := connectTestDB(t)
	store := NewPoolStore(pool, nil)
	db := muteDBName(t)
	base := time.Now().UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 1; i <= 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.PutMute(context.Background(), Mute{Database: db,
				Until: base.Add(time.Duration(i) * time.Hour), Actor: fmt.Sprintf("admin%d", i),
				Reason: "noisy"})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("PutMute: %v", err)
		}
	}
	var n int
	var until time.Time
	var actor string
	if err := pool.QueryRow(context.Background(), `SELECT count(*), max(until), max(actor)
		FROM sage.notification_mutes WHERE database = $1`, db).Scan(&n, &until,
		&actor); err != nil {
		t.Fatal(err)
	}
	if n != 1 || !until.Equal(base.Add(6*time.Hour)) || actor != "admin6" {
		t.Fatalf("rows %d until %v actor %q, want one row until +6h by admin6", n, until, actor)
	}
	got, err := store.PutMute(context.Background(), Mute{Database: db,
		Until: base.Add(time.Hour), Actor: "late"})
	if err != nil || !got.Until.Equal(base.Add(6*time.Hour)) || got.Actor != "admin6" {
		t.Fatalf("a shorter mute returned %+v, %v; want the +6h mute kept", got, err)
	}
}

// Boundary and state transitions on the real table: active strictly before
// until, a class mute matches its class only, unmute removes the row, an
// expired row is ignored without a sweep.
func TestMuteDB_ActiveBoundaryClassAndUnmute(t *testing.T) {
	pool := connectTestDB(t)
	store := NewPoolStore(pool, nil)
	ctx := context.Background()
	db := muteDBName(t)
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if _, err := store.PutMute(ctx, Mute{Database: db, Class: "ask", Until: until,
		Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	check := func(c Class, at time.Time, want bool) {
		t.Helper()
		got, err := store.ActiveMute(ctx, db, c, at)
		if err != nil || got != want {
			t.Fatalf("ActiveMute(%s, %v) = %v, %v; want %v", c, at, got, err, want)
		}
	}
	check(ClassAsk, until.Add(-time.Second), true)
	check(ClassAsk, until, false)
	check(ClassPage, until.Add(-time.Second), false)
	active, err := store.ListMutes(ctx, until.Add(-time.Minute))
	if err != nil || !containsMute(active, db, "ask") {
		t.Fatalf("active mutes %+v, %v", active, err)
	}
	expired, err := store.ListMutes(ctx, until.Add(time.Second))
	if err != nil || containsMute(expired, db, "ask") {
		t.Fatalf("an expired mute is listed as active: %+v, %v", expired, err)
	}
	removed, err := store.DeleteMute(ctx, db, "ask")
	if err != nil || !removed {
		t.Fatalf("DeleteMute = %v, %v", removed, err)
	}
	check(ClassAsk, until.Add(-time.Second), false)
}

func containsMute(ms []Mute, db, class string) bool {
	for _, m := range ms {
		if m.Database == db && m.Class == class {
			return true
		}
	}
	return false
}

// The table refuses what the API refuses: no database, an unknown class,
// a mute longer than 90 days or one that already ended.
func TestMuteDB_ChecksRefuseBadRows(t *testing.T) {
	pool := connectTestDB(t)
	db := muteDBName(t)
	for _, c := range []struct {
		database, class, until string
	}{
		{"", "", "now() + interval '1 hour'"},
		{db, "pager", "now() + interval '1 hour'"},
		{db, "", "now() + interval '91 days'"},
		{db, "", "now() - interval '1 second'"},
	} {
		_, err := pool.Exec(context.Background(), `INSERT INTO sage.notification_mutes
			(database, class, until, actor) VALUES ($1, $2, `+c.until+`, 'a')`,
			c.database, c.class)
		if err == nil {
			t.Errorf("mute %q/%q until %s was accepted", c.database, c.class, c.until)
		}
	}
}

func TestMuteDB_DispatcherMutesButNeverTheNeverMutedPages(t *testing.T) {
	o, seq := newOutboxDB(t, 200)
	db := muteDBName(t)
	insertTestRule(t, o.pool, o.ch, "action_failed", "info", true)
	d := o.dispatcher(t)
	ctx := context.Background()
	if _, err := o.store.PutMute(ctx, Mute{Database: db, Until: o.clock.Now().Add(time.Hour),
		Actor: "a"}); err != nil {
		t.Fatal(err)
	}
	muted := Event{Type: "finding_critical", Severity: "critical", Subject: "muted",
		Data: map[string]any{"database": db}, DedupKey: "m:" + db}
	if res, err := d.Submit(ctx, muted); err != nil || res.Suppressed != 1 {
		t.Fatalf("muted Submit = %+v, %v", res, err)
	}
	page := Event{Type: "action_failed", Severity: "warning", Subject: "rollback failed",
		Data: map[string]any{"database": db, DataRollbackFailed: true}, DedupKey: "r:" + db}
	if res, err := d.Submit(ctx, page); err != nil || res.Enqueued != 1 {
		t.Fatalf("never-muted Submit = %+v, %v", res, err)
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	seq.mu.Lock()
	defer seq.mu.Unlock()
	if len(seq.bodies) != 1 {
		t.Fatalf("delivered %d messages, want only the failed rollback", len(seq.bodies))
	}
}

func TestQuietDB_ChannelReadsItsQuietHours(t *testing.T) {
	o, _ := newOutboxDB(t, 200)
	ctx := context.Background()
	if _, err := o.pool.Exec(ctx, `UPDATE sage.notification_channels
		SET quiet_start = '22:00', quiet_end = '07:00', quiet_tz = 'America/Chicago'
		WHERE id = $1`, o.ch); err != nil {
		t.Fatal(err)
	}
	ch, err := o.store.Channel(ctx, o.ch)
	if err != nil {
		t.Fatal(err)
	}
	if ch.QuietStart != "22:00" || ch.QuietEnd != "07:00" || ch.QuietTZ != "America/Chicago" {
		t.Fatalf("channel quiet hours %q-%q %q", ch.QuietStart, ch.QuietEnd, ch.QuietTZ)
	}
	if _, err := o.pool.Exec(ctx, `UPDATE sage.notification_channels
		SET quiet_start = NULL, quiet_end = NULL, quiet_tz = NULL WHERE id = $1`,
		o.ch); err != nil {
		t.Fatal(err)
	}
	if ch, err = o.store.Channel(ctx, o.ch); err != nil || ch.QuietStart != "" {
		t.Fatalf("cleared quiet hours read %+v, %v", ch, err)
	}
}
