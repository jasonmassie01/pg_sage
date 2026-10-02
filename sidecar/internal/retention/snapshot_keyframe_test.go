package retention

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

func indexDoc(scans int) []byte {
	return []byte(fmt.Sprintf(`[{"schemaname":"app","relname":"t","indexrelname":"ix",`+
		`"idx_scan":%d,"indexdef":"CREATE INDEX ix ON app.t (c)"}]`, scans))
}

type keptRow struct {
	at     time.Time
	baseID *int64
	data   string
}

// Snapshot dedupe: a delta row is only readable with its keyframe. Retention
// deletes rows older than snapshots_days but keeps a keyframe while a
// retained delta references it; a keyframe referenced only by expired rows
// goes with them in the same run.
// No retained row is ever left without its keyframe.
func TestPurgeSnapshots_KeepsKeyframesOfRetainedDeltas(t *testing.T) {
	pool, ctx := requireDB(t)
	execRetry(t, ctx, `DELETE FROM sage.snapshots`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM sage.snapshots`) })
	now := time.Now().UTC()
	w := snapstore.NewWriter()
	k2 := now.Add(-30*24*time.Hour - 2*time.Hour)
	steps := []struct {
		at   time.Time
		scan int
	}{
		{now.Add(-40 * 24 * time.Hour), 1},           // K1, old
		{now.Add(-40*24*time.Hour + time.Minute), 2}, // delta on K1, old
		{k2, 3},                    // K2 (K1 is > 6 h old), old
		{k2.Add(time.Hour), 4},     // delta on K2, old
		{k2.Add(3 * time.Hour), 5}, // delta on K2, retained
	}
	for _, s := range steps {
		if err := w.Persist(ctx, pool, s.at, []snapstore.Row{
			{Category: "indexes", Data: indexDoc(s.scan)}}); err != nil {
			t.Fatalf("persist %s: %v", s.at, err)
		}
	}
	cfg := &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 30}}
	New(pool, cfg, noopLog).Run(ctx)
	kept := keptSnapshots(t)
	if len(kept) != 2 || !kept[0].at.Equal(k2.Truncate(time.Microsecond)) ||
		kept[0].baseID != nil || kept[1].baseID == nil {
		t.Fatalf("kept = %+v, want K2 and its retained delta", kept)
	}
	if kept[1].data != canonicalDoc(t, 5) {
		t.Fatalf("retained delta reads %s, want the scan-5 document", kept[1].data)
	}
	assertNoOrphans(t)

	// Once the last delta ages out, the keyframe goes in the same run.
	execRetry(t, ctx, `UPDATE sage.snapshots SET collected_at = now() - interval '31 days'
		WHERE base_id IS NOT NULL`)
	New(pool, cfg, noopLog).Run(ctx)
	if kept := keptSnapshots(t); len(kept) != 0 {
		t.Fatalf("kept = %+v after every reference aged out, want none", kept)
	}
}

func keptSnapshots(t *testing.T) []keptRow {
	t.Helper()
	rows, err := testPool.Query(t.Context(), `SELECT collected_at, base_id, `+
		snapstore.DataSQL("")+`::text FROM sage.snapshots WHERE category = 'indexes'
		ORDER BY collected_at`)
	if err != nil {
		t.Fatalf("kept rows: %v", err)
	}
	defer rows.Close()
	var out []keptRow
	for rows.Next() {
		var r keptRow
		var data *string
		if err := rows.Scan(&r.at, &r.baseID, &data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if data != nil {
			r.data = *data
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func canonicalDoc(t *testing.T, scans int) string {
	t.Helper()
	var s string
	queryRetry(t, t.Context(), fmt.Sprintf(`SELECT '%s'::jsonb::text`, indexDoc(scans)), &s)
	return s
}

func assertNoOrphans(t *testing.T) {
	t.Helper()
	var orphans int
	queryRetry(t, t.Context(), `SELECT count(*) FROM sage.snapshots d
		WHERE d.base_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM sage.snapshots b WHERE b.id = d.base_id)`, &orphans)
	if orphans != 0 {
		t.Fatalf("%d delta rows lost their keyframe", orphans)
	}
}
