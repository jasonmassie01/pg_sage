package schema

import (
	"context"
	"strings"
	"testing"
)

// lab-03a on a real server: bootstrap twice is clean and
// sage.action_queue gains a nullable ladder jsonb (NULL for existing rows).
// No concurrent-writer test: Bootstrap holds its advisory lock.
func TestActionQueueLadderMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "queueladder")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var typ, nullable string
	if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
		FROM information_schema.columns WHERE table_schema = 'sage'
		  AND table_name = 'action_queue' AND column_name = 'ladder'`).
		Scan(&typ, &nullable); err != nil {
		t.Fatalf("read the ladder column: %v", err)
	}
	if typ != "jsonb" || nullable != "YES" {
		t.Fatalf("ladder column = %s nullable %s, want nullable jsonb", typ, nullable)
	}
}

func TestActionQueueLadderStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"ADD COLUMN IF NOT EXISTS ladder jsonb",
		"SET LOCAL lock_timeout"} {
		if !strings.Contains(ddlActionQueueLadder, want) {
			t.Errorf("ddlActionQueueLadder lacks %q", want)
		}
	}
}
