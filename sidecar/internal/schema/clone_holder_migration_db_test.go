package schema

import (
	"context"
	"strings"
	"testing"
)

// lab-05a on a real server: bootstrap twice is clean, sage.clone_instances
// gains nullable holder_id, heartbeat_at and estimate, and a holder id is
// 1-200 characters. No concurrent-writer test: Bootstrap holds its
// advisory lock.
func TestCloneInstancesHolderMigration_BootstrapTwice(t *testing.T) {
	pool := freshSchemaPool(t, "cloneholder")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	want := map[string]string{"holder_id": "text", "heartbeat_at": "timestamp with time zone",
		"estimate": "jsonb"}
	for col, typ := range want {
		var got, nullable string
		if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable
			FROM information_schema.columns WHERE table_schema = 'sage'
			  AND table_name = 'clone_instances' AND column_name = $1`, col).
			Scan(&got, &nullable); err != nil {
			t.Fatalf("read column %s: %v", col, err)
		}
		if got != typ || nullable != "YES" {
			t.Errorf("%s = %s nullable %s, want nullable %s", col, got, nullable, typ)
		}
	}
	insert := `INSERT INTO sage.clone_instances (deployment_id, adapter, scope, name,
		purpose, status, expires_at, holder_id) VALUES
		('00000000-0000-4000-8000-000000000001', 'dle', 's', $1, 'rehearsal', 'creating',
		 now() + interval '1 hour', $2)`
	if _, err := pool.Exec(ctx, insert, "ok", "host/1/ab"); err != nil {
		t.Fatalf("a valid holder was refused: %v", err)
	}
	for name, holder := range map[string]string{"empty": "", "long": strings.Repeat("h", 201)} {
		if _, err := pool.Exec(ctx, insert, name, holder); err == nil ||
			!strings.Contains(err.Error(), "check") {
			t.Errorf("holder %s was accepted: %v", name, err)
		}
	}
}

func TestCloneInstancesHolderStep_IsIdempotentAndBounded(t *testing.T) {
	for _, want := range []string{"ADD COLUMN IF NOT EXISTS holder_id",
		"ADD COLUMN IF NOT EXISTS heartbeat_at", "ADD COLUMN IF NOT EXISTS estimate jsonb",
		"SET LOCAL lock_timeout"} {
		if !strings.Contains(ddlCloneInstancesHolder, want) {
			t.Errorf("ddlCloneInstancesHolder lacks %q", want)
		}
	}
}
