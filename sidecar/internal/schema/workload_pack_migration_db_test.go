package schema

import (
	"context"
	"strings"
	"testing"
)

// evid-02a on a real server: bootstrap twice is clean and writes no pack;
// the checks refuse another tier, a malformed hash and a body over 32 MB;
// one database and hash hold one row, also across placements' NULL id.
// No concurrent-writer test: Bootstrap holds its advisory lock (the
// concurrent build test is in workpack).

func TestWorkloadPacksMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "workpacks")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.workload_packs`).Scan(&n); err != nil ||
		n != 0 {
		t.Fatalf("workload_packs rows after bootstrap: %d (%v), want 0", n, err)
	}
	insert := func(dbID *int, tier, sha string, bytes int64) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.workload_packs (database_id, window_from,
			window_to, format, content_sha256, tier, bytes, built_at, built_by, manifest, body)
			VALUES ($1, '2026-10-05T00:00:00Z', '2026-11-02T00:00:00Z', 'pg_sage-workload/1',
			        $2, $3, $4, now(), 'op', '{}', '\x00')`, dbID, sha, tier, bytes)
		return err
	}
	sha := strings.Repeat("ab", 32)
	if err := insert(nil, "local", sha, 1); err != nil {
		t.Fatalf("a valid pack row was refused: %v", err)
	}
	if err := insert(nil, "local", sha, 1); err == nil ||
		!strings.Contains(err.Error(), "workload_packs_key") {
		t.Fatalf("a second row for one database and hash: %v, want the unique key", err)
	}
	seven := 7
	if err := insert(&seven, "local", sha, 1); err != nil {
		t.Fatalf("another database's pack with the same hash was refused: %v", err)
	}
	for name, err := range map[string]error{
		"model tier":     insert(nil, "model", strings.Repeat("cd", 32), 1),
		"upper-case sha": insert(nil, "local", strings.Repeat("CD", 32), 1),
		"short sha":      insert(nil, "local", "abc", 1),
		"over 32 MB":     insert(nil, "local", strings.Repeat("ef", 32), 33554433),
		"negative bytes": insert(nil, "local", strings.Repeat("01", 32), -1),
	} {
		if err == nil || !strings.Contains(err.Error(), "check") {
			t.Errorf("%s: %v, want a CHECK violation", name, err)
		}
	}
}
