package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ux-09a on real PostgreSQL: class rules live in sage.notification_rules
// (delivery_class), the Ask batch is decided under the outbox's advisory
// lock, and a first channel gets the class rules.

func newClassDB(t *testing.T) (*outboxDB, *statusSeq) {
	t.Helper()
	o, seq := newOutboxDB(t, 200)
	ctx := context.Background()
	if _, err := o.pool.Exec(ctx, `DELETE FROM sage.notification_rules WHERE channel_id = $1`,
		o.ch); err != nil {
		t.Fatal(err)
	}
	for _, c := range Classes() {
		if _, err := o.pool.Exec(ctx, `INSERT INTO sage.notification_rules
			(channel_id, event, min_severity, enabled, delivery_class)
			VALUES ($1, $2, 'info', true, $3)`, o.ch, ClassRuleEvent(c), string(c)); err != nil {
			t.Fatal(err)
		}
	}
	return o, seq
}

func (o *outboxDB) classDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	d := o.dispatcher(t)
	d.SetAskBatch(15 * time.Minute)
	return d
}

func TestClassDB_ClassRulesAreReadByClassOnly(t *testing.T) {
	o, _ := newClassDB(t)
	ctx := context.Background()
	rules, err := o.store.ClassRules(ctx, ClassAsk)
	if err != nil {
		t.Fatal(err)
	}
	var mine []Rule
	for _, r := range rules {
		if r.ChannelID == o.ch {
			mine = append(mine, r)
		}
	}
	if len(mine) != 1 || mine[0].DeliveryClass != "ask" || mine[0].Event != "class:ask" {
		t.Fatalf("ask class rules %+v", mine)
	}
	legacy, err := o.store.MatchingRules(ctx, "approval_needed")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range legacy {
		if r.ChannelID == o.ch {
			t.Fatalf("a class rule matched as an event rule: %+v", r)
		}
	}
}

// Two Ask events for one database at the same instant make one message.
func TestClassDB_ConcurrentAsksMakeOneBatch(t *testing.T) {
	o, seq := newClassDB(t)
	db := fmt.Sprintf("orders_%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = o.classDispatcher(t).Submit(context.Background(), askEvent(db, i+1))
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	pending := o.count(t, `SELECT count(*) FROM sage.notification_outbox
		WHERE channel_id = $1 AND status = 'pending'`)
	if pending != 1 {
		t.Fatalf("%d pending ask entries, want 1", pending)
	}
	o.clock.Advance(15 * time.Minute)
	if _, err := o.dispatcher(t).DrainOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, accepted := seq.counts(); accepted != 1 {
		t.Fatalf("%d messages, want one batch", accepted)
	}
	// The fixture's channel is Slack (not interactive, so it is batched);
	// the subject is in the message blocks.
	seq.mu.Lock()
	raw, err := json.Marshal(seq.bodies[0])
	seq.mu.Unlock()
	if err != nil || !strings.Contains(string(raw), "2 decisions waiting on "+db) {
		t.Fatalf("batch message %s (%v), want the two-decision subject", raw, err)
	}
}

// A first channel gets Page, Ask and Digest class rules; a second channel,
// or an install that already has rules, gets none.
func TestClassDB_SeedClassRulesOnlyForTheFirstChannel(t *testing.T) {
	pool := connectTestDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`DELETE FROM sage.notification_rules`,
		`DELETE FROM sage.notification_channels`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var first, second int
	if err := tx.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type, config)
		VALUES ('seed-first', 'slack', '{}') RETURNING id`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	n, err := SeedClassRules(ctx, tx, first)
	if err != nil || n != 3 {
		t.Fatalf("first channel seeded %d, %v; want 3", n, err)
	}
	var classes string
	if err := tx.QueryRow(ctx, `SELECT string_agg(delivery_class || '=' || event, ','
		ORDER BY delivery_class) FROM sage.notification_rules WHERE channel_id = $1`,
		first).Scan(&classes); err != nil {
		t.Fatal(err)
	}
	if classes != "ask=class:ask,digest=class:digest,page=class:page" {
		t.Fatalf("seeded rules %q", classes)
	}
	if n, err := SeedClassRules(ctx, tx, first); err != nil || n != 0 {
		t.Fatalf("a second seed added %d, %v; want 0 (idempotent)", n, err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type, config)
		VALUES ('seed-second', 'slack', '{}') RETURNING id`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if n, err := SeedClassRules(ctx, tx, second); err != nil || n != 0 {
		t.Fatalf("a second channel seeded %d, %v; want 0", n, err)
	}
}

func TestClassDB_SeedClassRulesSkipsAnInstallWithRules(t *testing.T) {
	pool := connectTestDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []string{`DELETE FROM sage.notification_rules`,
		`DELETE FROM sage.notification_channels`} {
		if _, err := tx.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var id int
	if err := tx.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type, config)
		VALUES ('seed-only', 'slack', '{}') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.notification_rules (channel_id, event)
		VALUES ($1, 'finding_critical')`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := SeedClassRules(ctx, tx, id); err != nil || n != 0 {
		t.Fatalf("an install with rules seeded %d, %v; want 0", n, err)
	}
}

// A pager (PagerDuty, Opsgenie, incident.io) as the first channel gets the
// Page and Ask class rules but no Digest rule: a daily summary must not
// page anyone (the same rule as the default digest rule, ux-02b).
func TestClassDB_SeedClassRulesGiveAPagerNoDigest(t *testing.T) {
	pool := connectTestDB(t)
	ctx := context.Background()
	for _, typ := range digestRuleExcludedTypes {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{`DELETE FROM sage.notification_rules`,
			`DELETE FROM sage.notification_channels`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		var id int
		if err := tx.QueryRow(ctx, `INSERT INTO sage.notification_channels
			(name, type, config) VALUES ('seed-pager', $1, '{}') RETURNING id`,
			typ).Scan(&id); err != nil {
			t.Fatal(err)
		}
		n, err := SeedClassRules(ctx, tx, id)
		var classes string
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT COALESCE(string_agg(delivery_class, ','
				ORDER BY delivery_class), '') FROM sage.notification_rules
				WHERE channel_id = $1`, id).Scan(&classes)
		}
		_ = tx.Rollback(ctx)
		if err != nil || n != 2 || classes != "ask,page" {
			t.Fatalf("%s first channel seeded %d (%q), %v; want ask and page only", typ,
				n, classes, err)
		}
	}
}
