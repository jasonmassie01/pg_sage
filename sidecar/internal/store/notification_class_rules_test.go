//go:build integration

package store

import (
	"context"
	"testing"
)

// ux-09a: the Rules tab reads a class rule as one (delivery_class), and a
// legacy rule of one event type as none. No concurrent access tests:
// ListRules is one read-only statement.
func TestListRules_ReportsEachRulesDeliveryClass(t *testing.T) {
	pool, ctx := requireDB(t)
	ns := NewNotificationStore(pool, nil)
	var ch, classRule, legacyRule int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type, config)
		VALUES ('class-list', 'slack', '{}') RETURNING id`).Scan(&ch); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.notification_channels WHERE id = $1`, ch)
	})
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_rules
		(channel_id, event, min_severity, delivery_class)
		VALUES ($1, 'class:ask', 'info', 'ask') RETURNING id`, ch).Scan(&classRule); err != nil {
		t.Fatalf("insert class rule: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_rules
		(channel_id, event, min_severity) VALUES ($1, 'finding_opened', 'info')
		RETURNING id`, ch).Scan(&legacyRule); err != nil {
		t.Fatalf("insert legacy rule: %v", err)
	}
	rules, err := ns.ListRules(ctx)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	got := map[int]string{}
	for _, r := range rules {
		got[r.ID] = r.DeliveryClass
	}
	if c, ok := got[classRule]; !ok || c != "ask" {
		t.Fatalf("class rule %d listed with class %q (present %v), want ask", classRule, c, ok)
	}
	if c, ok := got[legacyRule]; !ok || c != "" {
		t.Fatalf("legacy rule %d listed with class %q (present %v), want none", legacyRule,
			c, ok)
	}
}
