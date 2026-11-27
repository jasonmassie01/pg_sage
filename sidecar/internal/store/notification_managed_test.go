//go:build integration

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// alerting-06b: channels and rules the YAML reconciler owns are
// managed_by = 'config'. The API refuses to change or delete them, so the
// next reconcile never fights an operator's edit; API rows are unchanged.
// No concurrent access tests: each guard is one statement whose WHERE
// clause carries the managed_by check, so there is no read-then-write gap.

type managedRows struct {
	channel, rule int
}

func insertManaged(t *testing.T, pool *pgxpool.Pool, name string) managedRows {
	t.Helper()
	ctx := context.Background()
	var m managedRows
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels
		(name, type, config, managed_by, config_hash)
		VALUES ($1, 'slack', '{"webhook_url":"https://hooks.slack.com/services/T/B/c"}',
		        'config', 'h-channel') RETURNING id`, name).Scan(&m.channel); err != nil {
		t.Fatalf("insert config channel: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_rules
		(channel_id, event, min_severity, managed_by, config_hash)
		VALUES ($1, 'finding_opened', 'critical', 'config', 'h-rule') RETURNING id`,
		m.channel).Scan(&m.rule); err != nil {
		t.Fatalf("insert config rule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_channels WHERE id = $1`, m.channel)
	})
	return m
}

func channelState(t *testing.T, pool *pgxpool.Pool, id int) (name, hash string, on bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `SELECT name,
		COALESCE(config_hash, ''), enabled FROM sage.notification_channels WHERE id = $1`,
		id).Scan(&name, &hash, &on); err != nil {
		t.Fatalf("read channel %d: %v", id, err)
	}
	return name, hash, on
}

func TestManagedChannel_UpdateAndDeleteAreRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	m := insertManaged(t, pool, "config:slack:slack")
	err := ns.UpdateChannel(ctx, m.channel, "renamed",
		map[string]string{"webhook_url": "https://hooks.slack.com/services/T/B/other"}, false)
	if !errors.Is(err, ErrManagedByConfig) {
		t.Fatalf("UpdateChannel err = %v, want ErrManagedByConfig", err)
	}
	if name, hash, on := channelState(t, pool, m.channel); name != "config:slack:slack" ||
		hash != "h-channel" || !on {
		t.Fatalf("after a refused update: %q %q %v; want the row unchanged", name, hash, on)
	}
	if err := ns.DeleteChannel(ctx, m.channel); !errors.Is(err, ErrManagedByConfig) {
		t.Fatalf("DeleteChannel err = %v, want ErrManagedByConfig", err)
	}
	if _, hash, _ := channelState(t, pool, m.channel); hash != "h-channel" {
		t.Fatalf("config_hash after a refused delete = %q", hash)
	}
}

func TestManagedRule_WritesAreRefused(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	m := insertManaged(t, pool, "config:slack:rules")
	if err := ns.UpdateRule(ctx, m.rule, false); !errors.Is(err, ErrManagedByConfig) {
		t.Fatalf("UpdateRule err = %v, want ErrManagedByConfig", err)
	}
	if err := ns.DeleteRule(ctx, m.rule); !errors.Is(err, ErrManagedByConfig) {
		t.Fatalf("DeleteRule err = %v, want ErrManagedByConfig", err)
	}
	var on bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM sage.notification_rules
		WHERE id = $1`, m.rule).Scan(&on); err != nil || !on {
		t.Fatalf("rule after refused writes: enabled=%v err=%v; want unchanged", on, err)
	}
	// An API rule on a config channel would be deleted with the channel by
	// the next reconcile, so it is refused too.
	if _, err := ns.CreateRule(ctx, m.channel, "finding_opened", "warning"); !errors.Is(
		err, ErrManagedByConfig) {
		t.Fatalf("CreateRule on a config channel err = %v, want ErrManagedByConfig", err)
	}
}

func TestManagedRows_APIRowsStillChangeAndMissingIsNotFound(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	id := lastDeliveryChannel(t, ns, ctx, "a06b-api")
	if err := ns.UpdateChannel(ctx, id, "a06b-api", map[string]string{
		"webhook_url": "https://hooks.slack.com/services/T/B/n"}, false); err != nil {
		t.Fatalf("UpdateChannel on an API row: %v", err)
	}
	ruleID, err := ns.CreateRule(ctx, id, "finding_opened", "warning")
	if err != nil {
		t.Fatalf("CreateRule on an API channel: %v", err)
	}
	if err := ns.UpdateRule(ctx, ruleID, false); err != nil {
		t.Fatalf("UpdateRule on an API rule: %v", err)
	}
	if err := ns.DeleteRule(ctx, ruleID); err != nil {
		t.Fatalf("DeleteRule on an API rule: %v", err)
	}
	if err := ns.DeleteRule(ctx, ruleID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteRule err = %v, want ErrNotFound", err)
	}
	if err := ns.DeleteChannel(ctx, 2147483000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteChannel of a missing id err = %v, want ErrNotFound", err)
	}
}

func TestManagedRows_ConfigPrefixIsReservedForTheReconciler(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	_, err := ns.CreateChannel(ctx, "config:slack:mine", "slack",
		map[string]string{"webhook_url": "https://hooks.slack.com/services/T/B/x"}, 1)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("CreateChannel config: name err = %v, want ErrValidation", err)
	}
	id := lastDeliveryChannel(t, ns, ctx, "a06b-rename")
	err = ns.UpdateChannel(ctx, id, "config:slack:mine",
		map[string]string{"webhook_url": "https://hooks.slack.com/services/T/B/x"}, true)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("rename into config: err = %v, want ErrValidation", err)
	}
	if name, _, _ := channelState(t, pool, id); name != "a06b-rename" {
		t.Fatalf("name after a refused rename = %q", name)
	}
}

func TestManagedRows_ListingsCarryManagedBy(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	m := insertManaged(t, pool, "config:slack:list")
	api := lastDeliveryChannel(t, ns, ctx, "a06b-list")
	got := listingByID(t, ns, ctx)
	if got[m.channel].ManagedBy != "config" || got[api].ManagedBy != "api" {
		t.Fatalf("managed_by = %q / %q, want config / api",
			got[m.channel].ManagedBy, got[api].ManagedBy)
	}
	rules, err := ns.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	for _, r := range rules {
		if r.ID == m.rule && r.ManagedBy != "config" {
			t.Fatalf("rule %d managed_by = %q, want config", r.ID, r.ManagedBy)
		}
	}
}
