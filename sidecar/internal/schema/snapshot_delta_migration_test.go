package schema

import (
	"strings"
	"testing"
)

// Snapshot dedupe: sage.snapshots gains base_id (a delta row's keyframe),
// a partial index for retention's "is this keyframe still referenced"
// check, and the SQL accessor that turns any row back into the document
// the collector wrote. Idempotent; existing rows are not rewritten.
func TestSnapshotDeltaMigration_ColumnIndexFunctions(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	var colType string
	if err := pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
		WHERE table_schema = 'sage' AND table_name = 'snapshots'
		  AND column_name = 'base_id'`).Scan(&colType); err != nil || colType != "bigint" {
		t.Fatalf("base_id column = %q (%v), want bigint", colType, err)
	}
	var indexDef string
	if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'sage' AND tablename = 'snapshots'
		  AND indexdef LIKE '%(base_id)%'`).Scan(&indexDef); err != nil {
		t.Fatalf("base_id index missing: %v", err)
	}
	if want := "WHERE (base_id IS NOT NULL)"; !strings.Contains(indexDef, want) {
		t.Fatalf("index = %s, want partial %s", indexDef, want)
	}
	var fns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'sage'
		  AND p.proname IN ('snapshot_apply', 'snapshot_data')`).Scan(&fns); err != nil ||
		fns != 2 {
		t.Fatalf("accessor functions = %d (%v), want 2", fns, err)
	}
}

// sage.snapshot_apply decodes every delta form: field patches by key,
// removals, appended elements, an explicit order, composite and integer
// keys, an empty result, and a missing base (NULL, never a guess).
func TestSnapshotApply_Semantics(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	base := `[{"k":"a","x":1},{"k":"b","x":2}]`
	cases := []struct {
		name, base, delta, want string
	}{
		{"patch", base, `{"k":["k"],"n":2,"u":{"b":{"x":3,"y":null}}}`,
			`[{"k": "a", "x": 1}, {"k": "b", "x": 3, "y": null}]`},
		{"no change", base, `{"k":["k"],"n":2}`, `[{"k": "a", "x": 1}, {"k": "b", "x": 2}]`},
		{"remove and append", base, `{"k":["k"],"n":2,"d":["a"],"a":[{"k":"c"}]}`,
			`[{"k": "b", "x": 2}, {"k": "c"}]`},
		{"explicit order", base, `{"k":["k"],"n":2,"a":[{"k":"c"}],"o":[2,0]}`,
			`[{"k": "c"}, {"k": "a", "x": 1}]`},
		{"order with patch", base, `{"k":["k"],"n":2,"u":{"a":{"x":9}},"o":[1,0]}`,
			`[{"k": "b", "x": 2}, {"k": "a", "x": 9}]`},
		{"composite key",
			`[{"t":"x","c":"f","col":"a","v":0},{"t":"x","c":"f","col":"b","v":0}]`,
			`{"k":["t","c","col"],"n":2,"u":{"x\u001ff\u001fb":{"v":1}}}`,
			`[{"c": "f", "t": "x", "v": 0, "col": "a"}, {"c": "f", "t": "x", "v": 1, "col": "b"}]`},
		{"integer key", `[{"queryid":9007199254740993,"calls":1}]`,
			`{"k":["queryid"],"n":1,"u":{"9007199254740993":{"calls":2}}}`,
			`[{"calls": 2, "queryid": 9007199254740993}]`},
		{"empty result", base, `{"k":["k"],"n":0,"d":["a","b"]}`, `[]`},
		{"empty base", `[]`, `{"k":["k"],"n":1,"a":[{"k":"z"}]}`, `[{"k": "z"}]`},
	}
	for _, tc := range cases {
		var got string
		err := pool.QueryRow(ctx, `SELECT sage.snapshot_apply($1::jsonb, $2::jsonb)::text`,
			tc.base, tc.delta).Scan(&got)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %s (%v)\n want %s", tc.name, got, err, tc.want)
		}
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT sage.snapshot_apply(NULL, '{"k":["k"]}') IS NULL`).
		Scan(&isNull); err != nil || !isNull {
		t.Fatalf("NULL base: null=%v (%v), want NULL", isNull, err)
	}
}

// sage.snapshot_data returns a full row as stored, decodes a delta row
// against its keyframe, and returns NULL when the keyframe is gone.
func TestSnapshotData_FullDeltaAndMissingBase(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	var baseID int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '400 days', 'migration_test', '[{"k":"a","x":1}]')
		RETURNING id`).Scan(&baseID); err != nil {
		t.Fatalf("insert keyframe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.snapshots WHERE category = 'migration_test'`)
	})
	var full, delta, orphan *string
	err := pool.QueryRow(ctx, `SELECT
		sage.snapshot_data('[1]', NULL)::text,
		sage.snapshot_data('{"k":["k"],"n":1,"u":{"a":{"x":2}}}', $1)::text,
		sage.snapshot_data('{"k":["k"],"n":1}', -1)::text`, baseID).
		Scan(&full, &delta, &orphan)
	if err != nil {
		t.Fatalf("snapshot_data: %v", err)
	}
	if full == nil || *full != "[1]" || delta == nil || *delta != `[{"k": "a", "x": 2}]` ||
		orphan != nil {
		t.Fatalf("full=%v delta=%v orphan=%v", deref(full), deref(delta), deref(orphan))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return *s
}
