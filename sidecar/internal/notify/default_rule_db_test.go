package notify

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// ux-02b: a default digest_ready rule goes on the first configured channel
// that suits a digest when no digest_ready rule exists, so an owner who
// already set up Slack or email gets the digest without a new rule. A
// pager (PagerDuty, Opsgenie, incident.io) never gets one: a daily
// summary must not page anyone.

func defaultRuleDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "digestrule"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func digestRules(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT c.name || '|' || r.min_severity
		|| '|' || r.enabled::text FROM sage.notification_rules r
		JOIN sage.notification_channels c ON c.id = r.channel_id
		WHERE r.event = 'digest_ready' ORDER BY r.id`)
	if err != nil {
		t.Fatalf("read digest rules: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func TestEnsureDefaultRule_FirstSuitableChannelOnce(t *testing.T) {
	pool := defaultRuleDB(t)
	insertTestChannel(t, pool, "pager", "pagerduty", map[string]string{"routing_key": "k"},
		true)
	insertTestChannel(t, pool, "off-slack", "slack", map[string]string{"webhook_url": "x"},
		false)
	insertTestChannel(t, pool, "team-mail", "email", map[string]string{"to": "a@b.test"},
		true)
	insertTestChannel(t, pool, "later-slack", "slack", map[string]string{"webhook_url": "y"},
		true)
	store := NewPoolStore(pool, nil)
	created, err := store.EnsureDefaultRule(context.Background(), EventDigestReady)
	if err != nil || !created {
		t.Fatalf("EnsureDefaultRule = %v, %v; want created", created, err)
	}
	if got := strings.Join(digestRules(t, pool), ","); got != "team-mail|info|true" {
		t.Fatalf("digest rules = %s, want one info rule on team-mail", got)
	}
	again, err := store.EnsureDefaultRule(context.Background(), EventDigestReady)
	if err != nil || again {
		t.Fatalf("second EnsureDefaultRule = %v, %v; want nothing created", again, err)
	}
}

// State transition: a disabled rule the owner kept is a rule; no default
// is added beside it.
func TestEnsureDefaultRule_KeepsTheOwnersDisabledRule(t *testing.T) {
	pool := defaultRuleDB(t)
	id := insertTestChannel(t, pool, "team-slack", "slack",
		map[string]string{"webhook_url": "x"}, true)
	insertTestRule(t, pool, id, EventDigestReady, "info", false)
	created, err := NewPoolStore(pool, nil).EnsureDefaultRule(context.Background(),
		EventDigestReady)
	if err != nil || created {
		t.Fatalf("EnsureDefaultRule = %v, %v; want no rule added", created, err)
	}
	if got := strings.Join(digestRules(t, pool), ","); got != "team-slack|info|false" {
		t.Fatalf("digest rules = %s, want only the owner's disabled rule", got)
	}
}

// Nil/empty: no channel, or only pagers, creates nothing.
func TestEnsureDefaultRule_NoSuitableChannel(t *testing.T) {
	pool := defaultRuleDB(t)
	store := NewPoolStore(pool, nil)
	if created, err := store.EnsureDefaultRule(context.Background(), EventDigestReady); err !=
		nil || created {
		t.Fatalf("no channels: %v, %v; want nothing", created, err)
	}
	insertTestChannel(t, pool, "pager", "opsgenie", map[string]string{"api_key": "k"}, true)
	insertTestChannel(t, pool, "inc", "incidentio", map[string]string{"api_key": "k"}, true)
	if created, err := store.EnsureDefaultRule(context.Background(), EventDigestReady); err !=
		nil || created {
		t.Fatalf("pagers only: %v, %v; want nothing", created, err)
	}
	if got := digestRules(t, pool); len(got) != 0 {
		t.Fatalf("digest rules = %v, want none", got)
	}
}

// Invalid input and error propagation: an unknown event is refused, and a
// store without a pool says so.
func TestEnsureDefaultRule_RefusesBadInput(t *testing.T) {
	if _, err := (&PoolStore{}).EnsureDefaultRule(context.Background(),
		EventDigestReady); err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("nil pool err = %v, want one naming the pool", err)
	}
	pool := defaultRuleDB(t)
	if _, err := NewPoolStore(pool, nil).EnsureDefaultRule(context.Background(),
		"not_an_event"); err == nil || !strings.Contains(err.Error(), "not_an_event") {
		t.Fatalf("unknown event err = %v, want one naming it", err)
	}
}

// Concurrency: callers racing on an empty rule table add one rule.
func TestEnsureDefaultRule_ConcurrentCallersAddOne(t *testing.T) {
	pool := defaultRuleDB(t)
	insertTestChannel(t, pool, "team-slack", "slack", map[string]string{"webhook_url": "x"},
		true)
	store := NewPoolStore(pool, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.EnsureDefaultRule(context.Background(),
				EventDigestReady); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("EnsureDefaultRule: %v", err)
	}
	if got := digestRules(t, pool); len(got) != 1 {
		t.Fatalf("digest rules = %v, want exactly one", got)
	}
}
