package siem

import (
	"context"
	"testing"
)

// oldVersionSaveSQL is the cursor upsert of a sidecar from before cursors
// kept hashes: it moves seq and leaves the hash column alone.
const oldVersionSaveSQL = `INSERT INTO sage.siem_cursor (sink, source, chain, seq)
	VALUES ($1, 'orders', 'action_log', $2) ON CONFLICT (sink, source, chain)
	DO UPDATE SET seq = EXCLUDED.seq, updated_at = now()
	WHERE sage.siem_cursor.seq < EXCLUDED.seq`

// In a rolling upgrade an older sidecar can lead after a newer one saved a
// hash. Its save moves the cursor without a hash, so the stale hash must go
// with it: otherwise the next new leader compares link 8 with link 5's hash
// and stops a healthy chain as regressed.
func TestOldVersionSaveDropsTheStaleHash(t *testing.T) {
	pool := freshDB(t, "siem_mixed_version")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 5)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 5 || hash != linkHash(t, pool, 5) {
		t.Fatalf("cursor = %d %q, want link 5 and its hash", seq, hash)
	}
	addActions(t, pool, 3)
	mustExec(t, pool, oldVersionSaveSQL, "soc", int64(8))
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 8 || hash != "" {
		t.Fatalf("after an old-version save: cursor = %d %q, want 8 without a hash",
			seq, hash)
	}
	before := len(sink.exported())
	addActions(t, pool, 2)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after the old leader: %v", err)
	}
	if st := e.Status()[0]; st.Status != StatusOK || len(st.Regressions) != 0 {
		t.Fatalf("a healthy chain after an old-version save = %+v, want ok", st)
	}
	got := since(sink.exported(), before)
	if len(got) != 2 || got[0].seq != 9 || got[1].seq != 10 {
		t.Fatalf("delivered %+v, want links 9 and 10", got)
	}
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 10 || hash != linkHash(t, pool, 10) {
		t.Fatalf("cursor = %d %q, want link 10 and its hash", seq, hash)
	}
}

// The guard only drops a hash the update did not set: the new version's
// saves, backfills and re-anchors keep theirs, and an update that leaves
// seq alone keeps the hash.
func TestCursorHashGuardKeepsWrittenHashes(t *testing.T) {
	pool := freshDB(t, "siem_hash_guard")
	ctx := context.Background()
	c := NewCursorStore(pool)
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 4, "h4"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 6, "h6"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 6 || hash != "h6" {
		t.Fatalf("cursor = %d %q, want 6 h6", seq, hash)
	}
	mustExec(t, pool, `UPDATE sage.siem_cursor SET updated_at = now() WHERE sink = 'soc'`)
	if _, hash, _ := cursorRow(t, pool, "soc"); hash != "h6" {
		t.Fatalf("an update without a move dropped the hash: %q", hash)
	}
	if _, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "action_log", 3,
		"h3"); err != nil {
		t.Fatalf("re-anchor: %v", err)
	}
	if seq, hash, forkTo := cursorRow(t, pool, "soc"); seq != 3 || hash != "h3" ||
		forkTo != 6 {
		t.Fatalf("re-anchored cursor = %d %q fork_to %d, want 3 h3 6", seq, hash, forkTo)
	}
	mustExec(t, pool, oldVersionSaveSQL, "soc", int64(5))
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 5 || hash != "" {
		t.Fatalf("old-version save = %d %q, want 5 without a hash", seq, hash)
	}
	if err := c.BackfillHash(ctx, Fence{}, "soc", "orders", "action_log", 5,
		"h5"); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 5 || hash != "h5" {
		t.Fatalf("backfilled cursor = %d %q, want 5 h5", seq, hash)
	}
}
