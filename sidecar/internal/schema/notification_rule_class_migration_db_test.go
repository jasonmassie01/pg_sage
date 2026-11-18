package schema

import (
	"context"
	"testing"
)

// ux-09a: delivery_class on a real server. Bootstrap twice is clean, the
// check refuses an unknown class, and NULL (a legacy rule) stays allowed.
// No concurrent-writer test: Bootstrap holds its advisory lock.

func TestNotificationRuleClassMigration_BootstrapTwiceAndCheck(t *testing.T) {
	pool := freshSchemaPool(t, "ruleclass")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
		if _, err := pool.Exec(ctx, ddlNotificationRuleClass); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
	}
	var ch int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.notification_channels (name, type, config)
		VALUES ('c', 'slack', '{}') RETURNING id`).Scan(&ch); err != nil {
		t.Fatal(err)
	}
	insert := func(event string, class any) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.notification_rules
			(channel_id, event, delivery_class) VALUES ($1, $2, $3)`, ch, event, class)
		return err
	}
	for _, c := range []string{"page", "ask", "digest"} {
		if err := insert("class:"+c, c); err != nil {
			t.Fatalf("class %s refused: %v", c, err)
		}
	}
	if err := insert("finding_critical", nil); err != nil {
		t.Fatalf("a legacy rule (NULL class) was refused: %v", err)
	}
	for _, bad := range []string{"pager", "PAGE", ""} {
		if err := insert("class:"+bad, bad); err == nil {
			t.Fatalf("class %q was accepted", bad)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes
		WHERE indexname = 'idx_notification_rules_class'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("class index count %d, %v", n, err)
	}
}
