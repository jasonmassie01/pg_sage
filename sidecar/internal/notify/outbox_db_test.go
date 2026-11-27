package notify

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-06 M2/M3 on real PostgreSQL: the outbox and the per-channel
// state live in sage.notification_outbox and sage.notification_state on
// the control pool; two dispatchers sharing it never deliver twice.

type outboxDB struct {
	pool  *pgxpool.Pool
	store *PoolStore
	clock *fakeClock
	ch    int
}

func newOutboxDB(t *testing.T, statuses ...int) (*outboxDB, *statusSeq) {
	t.Helper()
	pool := connectTestDB(t)
	seq := &statusSeq{statuses: statuses}
	srv := seq.server(t)
	tag := fmt.Sprintf("a06_%d", time.Now().UnixNano())
	o := &outboxDB{pool: pool, store: NewPoolStore(pool, nil),
		clock: &fakeClock{now: time.Now().UTC().Truncate(time.Second)}}
	o.ch = insertTestChannel(t, pool, tag, "slack", map[string]string{"webhook_url": srv.URL}, true)
	insertTestRule(t, pool, o.ch, "finding_opened", "info", true)
	insertTestRule(t, pool, o.ch, "finding_critical", "info", true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, q := range []string{
			`DELETE FROM sage.notification_log WHERE channel_id = $1`,
			`DELETE FROM sage.notification_state WHERE channel_id = $1`,
			`DELETE FROM sage.notification_channels WHERE id = $1`,
		} {
			_, _ = pool.Exec(ctx, q, o.ch)
		}
	})
	return o, seq
}

func (o *outboxDB) dispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	d := NewDefaultDispatcher(o.store, TargetPolicy{AllowPrivate: true}, nil)
	d.now = o.clock.Now
	d.UseOutbox(o.store)
	d.SetAlertPolicy(testPolicy(t, "", ""))
	return d
}

func (o *outboxDB) count(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := o.pool.QueryRow(context.Background(), q, o.ch).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestOutboxDB_RetryThenDeliverOnceAcrossTwoDispatchers(t *testing.T) {
	o, seq := newOutboxDB(t, 503, 200)
	a, b := o.dispatcher(t), o.dispatcher(t)
	ctx := context.Background()
	res, err := a.Submit(ctx, findingEvt("critical"))
	if err != nil || res.Enqueued != 1 {
		t.Fatalf("Submit = %+v, %v; want enqueued=1", res, err)
	}
	if res, _ := b.Submit(ctx, findingEvt("critical")); res.Suppressed != 1 {
		t.Fatalf("second sidecar Submit = %+v, want suppressed by the shared state", res)
	}
	if got, err := a.DrainOutbox(ctx); err != nil || got.Retried != 1 {
		t.Fatalf("first drain = %+v, %v; want a retry", got, err)
	}
	o.clock.Advance(outboxBackoff(1))
	var wg sync.WaitGroup
	for _, d := range []*Dispatcher{a, b} {
		wg.Add(1)
		go func(d *Dispatcher) {
			defer wg.Done()
			if _, err := d.DrainOutbox(ctx); err != nil {
				t.Errorf("concurrent drain: %v", err)
			}
		}(d)
	}
	wg.Wait()
	if hits, accepted := seq.counts(); hits != 2 || accepted != 1 {
		t.Fatalf("hits=%d accepted=%d, want 2 and 1", hits, accepted)
	}
	if n := o.count(t, `SELECT count(*) FROM sage.notification_outbox
		WHERE channel_id = $1 AND status = 'sent' AND attempts = 2`); n != 1 {
		t.Fatalf("sent outbox rows with 2 attempts = %d, want 1", n)
	}
	if n := o.count(t, `SELECT count(*) FROM sage.notification_state
		WHERE channel_id = $1 AND database = 'db1' AND open`); n != 1 {
		t.Fatalf("open state rows = %d, want 1", n)
	}
	if n := o.count(t, `SELECT count(*) FROM sage.notification_log
		WHERE channel_id = $1 AND status IN ('retrying', 'sent')`); n != 2 {
		t.Fatalf("log rows = %d, want a retrying and a sent row", n)
	}
}

func TestOutboxDB_LegacyResolveWithoutStateIsDelivered(t *testing.T) {
	// An incident paged by the old synchronous path has no state row; its
	// resolve still goes out under the key it was opened with.
	o, seq := newOutboxDB(t, 200)
	d := o.dispatcher(t)
	ctx := context.Background()
	legacy := FindingCriticalEvent("Bloat", "x", "db1", "bloat", "t")
	if res, err := d.Submit(ctx, ResolvedEvent(legacy)); err != nil || res.Enqueued != 1 {
		t.Fatalf("Submit resolve = %+v, %v; want enqueued", res, err)
	}
	if got, err := d.DrainOutbox(ctx); err != nil || got.Sent != 1 {
		t.Fatalf("drain = %+v, %v; want sent", got, err)
	}
	if _, accepted := seq.counts(); accepted != 1 {
		t.Fatalf("accepted = %d, want the resolve", accepted)
	}
	var key string
	if err := o.pool.QueryRow(ctx, `SELECT dedup_key FROM sage.notification_outbox
		WHERE channel_id = $1`, o.ch).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "finding_critical:db1:bloat:t" {
		t.Fatalf("outbox dedup_key = %q, want the legacy key", key)
	}
}

func TestOutboxDB_ReminderAndQuietDeferralPersist(t *testing.T) {
	o, seq := newOutboxDB(t, 200)
	d := o.dispatcher(t)
	ctx := context.Background()
	if _, err := d.Submit(ctx, findingEvt("warning")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	o.clock.Advance(60 * time.Minute)
	got, err := d.DrainOutbox(ctx)
	if err != nil || got.Reminders != 1 {
		t.Fatalf("drain at the interval = %+v, %v; want one reminder", got, err)
	}
	if _, err := d.DrainOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if _, accepted := seq.counts(); accepted != 2 {
		t.Fatalf("accepted = %d, want the alert and one reminder", accepted)
	}
	if n := o.count(t, `SELECT reminders FROM sage.notification_state
		WHERE channel_id = $1`); n != 1 {
		t.Fatalf("stored reminders = %d, want 1", n)
	}
}
