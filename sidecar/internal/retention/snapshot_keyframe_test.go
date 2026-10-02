package retention

import (
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// indexDoc is a catalog of 20 indexes: hot scans of index 0, warm scans
// of indexes 1-10. Large enough that a few moved counters are a small
// delta (a delta over half its keyframe is written in full instead).
func indexDoc(hot, warm int) []byte {
	doc := []byte("[")
	for i := 0; i < 20; i++ {
		scans := 0
		switch {
		case i == 0:
			scans = hot
		case i <= 10:
			scans = warm
		}
		if i > 0 {
			doc = append(doc, ',')
		}
		doc = fmt.Appendf(doc, `{"schemaname":"app","relname":"t","indexrelname":"ix%02d",`+
			`"idx_scan":%d,"indexdef":"CREATE INDEX ix%02d ON app.t (c%02d)"}`, i, scans, i, i)
	}
	return append(doc, ']')
}

type keptRow struct {
	at     time.Time
	baseID *int64
	data   string
}

// Snapshot dedupe: a delta row is only readable with its base, and a base
// may be a checkpoint on a keyframe. Retention deletes rows older than
// snapshots_days but keeps every row a retained row is built on; a base
// used only by expired rows goes with them in the same run. No retained
// row is ever left without its base.
func TestPurgeSnapshots_KeepsBasesOfRetainedDeltas(t *testing.T) {
	pool, ctx := requireDB(t)
	execRetry(t, ctx, `DELETE FROM sage.snapshots`)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM sage.snapshots`) })
	now := time.Now().UTC()
	w := snapstore.NewWriter()
	k1, k2 := now.Add(-40*24*time.Hour), now.Add(-30*24*time.Hour-2*time.Hour)
	steps := []struct {
		at        time.Time
		hot, warm int
	}{
		{k1, 0, 0},                    // K1, old
		{k1.Add(time.Minute), 1, 0},   // checkpoint on K1, old
		{k2, 0, 0},                    // K2 (K1 is > 6 h old), old
		{k2.Add(time.Hour), 0, 7},     // checkpoint on K2 (ten indexes moved), old
		{k2.Add(3 * time.Hour), 1, 7}, // delta on that checkpoint, retained
	}
	for _, s := range steps {
		if err := w.Persist(ctx, pool, s.at, []snapstore.Row{
			{Category: "indexes", Data: indexDoc(s.hot, s.warm)}}); err != nil {
			t.Fatalf("persist %s: %v", s.at, err)
		}
	}
	cfg := &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 30}}
	New(pool, cfg, noopLog).Run(ctx)
	kept := keptSnapshots(t)
	if len(kept) != 3 || !kept[0].at.Equal(k2.Truncate(time.Microsecond)) ||
		kept[0].baseID != nil || kept[1].baseID == nil || kept[2].baseID == nil {
		t.Fatalf("kept = %+v, want K2, its checkpoint and the retained delta", kept)
	}
	if kept[2].data != canonicalDoc(t, 1, 7) || kept[1].data != canonicalDoc(t, 0, 7) {
		t.Fatalf("kept rows read %s / %s", kept[1].data, kept[2].data)
	}
	assertNoOrphans(t)

	// Once the last delta ages out, its bases go in the same run.
	execRetry(t, ctx, `UPDATE sage.snapshots SET collected_at = now() - interval '31 days'
		WHERE collected_at > now() - interval '30 days'`)
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

func canonicalDoc(t *testing.T, hot, warm int) string {
	t.Helper()
	var s string
	queryRetry(t, t.Context(), fmt.Sprintf(`SELECT '%s'::jsonb::text`, indexDoc(hot, warm)),
		&s)
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
