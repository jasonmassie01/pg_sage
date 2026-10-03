package snapstore

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// Rows written on a day partition read back through the accessor, and a
// new UTC day starts with a keyframe stored in that day's partition.
func TestPersist_DayPartitionsHoldTheirOwnBases(t *testing.T) {
	pool, ctx := requireDB(t)
	day := partition.DayStart(time.Now()).Add(24 * time.Hour) // a daily partition
	if _, err := partition.Ensure(ctx, pool, partition.Snapshots, day, 2); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	w := NewWriter()
	steps := []time.Time{day.Add(23*time.Hour + 58*time.Minute),
		day.Add(23*time.Hour + 59*time.Minute), day.Add(24 * time.Hour)}
	docs := [][]byte{list(idx("a", 1), idx("b", 2)), list(idx("a", 1), idx("b", 3)),
		list(idx("a", 2), idx("b", 3))}
	for i, at := range steps {
		row := []Row{{Category: "indexes", Data: docs[i]}}
		if err := w.Persist(ctx, pool, at, row); err != nil {
			t.Fatalf("persist %s: %v", at, err)
		}
	}
	rows := storedRows(t, ctx, pool, "indexes")
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].baseID != nil || rows[1].baseID == nil || *rows[1].baseID != rows[0].id ||
		rows[2].baseID != nil {
		t.Fatalf("bases = %v %v %v; want keyframe, delta on it, keyframe on the new day",
			rows[0].baseID, rows[1].baseID, rows[2].baseID)
	}
	for i, r := range rows {
		if r.data != canonical(t, ctx, pool, docs[i]) {
			t.Fatalf("row %d reads %s", i, r.data)
		}
	}
	var where string
	if err := pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM sage.snapshots
		WHERE id = $1`, rows[2].id).Scan(&where); err != nil {
		t.Fatal(err)
	}
	if want := "sage." + partition.Snapshots.DayName(steps[2]); where != want {
		t.Fatalf("new day's keyframe stored in %s, want %s", where, want)
	}
}

// Rows written before this release may name a base in an earlier day (the
// history partition). The accessor falls back to a lookup by id alone.
func TestSnapshotData_FindsABaseInAnEarlierDay(t *testing.T) {
	pool, ctx := requireDB(t)
	day := partition.DayStart(time.Now()).Add(24 * time.Hour)
	if _, err := partition.Ensure(ctx, pool, partition.Snapshots, day, 2); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	full := list(idx("a", 1), idx("b", 2))
	var baseID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '3 days', 'indexes', $1) RETURNING id`, string(full)).
		Scan(&baseID); err != nil {
		t.Fatal(err)
	}
	base, err := parseCatalog(full, keyFields["indexes"])
	if err != nil {
		t.Fatal(err)
	}
	next, err := parseCatalog(list(idx("a", 1), idx("b", 5)), keyFields["indexes"])
	if err != nil {
		t.Fatal(err)
	}
	delta, err := encodeDelta(base, next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data, base_id)
		VALUES ($1, 'indexes', $2, $3)`, day.Add(time.Hour), string(delta), baseID); err != nil {
		t.Fatal(err)
	}
	rows := storedRows(t, ctx, pool, "indexes")
	if len(rows) != 2 || rows[1].data != canonical(t, ctx, pool, list(idx("a", 1), idx("b", 5))) {
		t.Fatalf("rows = %+v", rows)
	}
	// The two-argument accessor documented for operators still works.
	var legacy string
	if err := pool.QueryRow(ctx, `SELECT sage.snapshot_data(data, base_id)::text
		FROM sage.snapshots WHERE base_id = $1`, baseID).Scan(&legacy); err != nil ||
		legacy != rows[1].data {
		t.Fatalf("two-argument accessor = %q, %v", legacy, err)
	}
}
