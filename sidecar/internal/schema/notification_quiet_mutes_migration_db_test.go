package schema

import (
	"context"
	"testing"
)

// ux-09b on a real server: bootstrap twice is clean, the new columns and
// the mutes table exist, and the mutes table keeps one row per database
// and class. No concurrent-writer test: Bootstrap holds its advisory lock.

func TestNotificationQuietAndMutesMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "quietmutes")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
		if _, err := pool.Exec(ctx, ddlNotificationQuietAndMutes); err != nil {
			t.Fatalf("step run %d: %v", run, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'sage' AND (
		 (table_name = 'notification_channels'
		  AND column_name IN ('quiet_start', 'quiet_end', 'quiet_tz'))
		 OR (table_name = 'notification_log' AND column_name = 'item_refs')
		 OR (table_name = 'action_queue' AND column_name = 'snoozed_at'))`).
		Scan(&cols); err != nil || cols != 5 {
		t.Fatalf("new columns %d, %v; want 5", cols, err)
	}
	insert := func(class string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.notification_mutes
			(database, class, until, actor) VALUES ('orders', $1,
			now() + interval '1 hour', 'a')`, class)
		return err
	}
	if err := insert(""); err != nil {
		t.Fatalf("a database mute was refused: %v", err)
	}
	if err := insert("ask"); err != nil {
		t.Fatalf("a class mute was refused: %v", err)
	}
	if err := insert(""); err == nil {
		t.Fatal("a second mute of the same database and class was accepted")
	}
}
