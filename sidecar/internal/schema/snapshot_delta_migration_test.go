package schema

import (
	"strconv"
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
		{"values", base, `{"n":2,"u":{"1":{"x":3,"y":null}}}`,
			`[{"k": "a", "x": 1}, {"k": "b", "x": 3, "y": null}]`},
		{"no change", base, `{"n":2}`, `[{"k": "a", "x": 1}, {"k": "b", "x": 2}]`},
		{"increments and values", base, `{"n":2,"i":{"1":{"x":-2}},"u":{"0":{"y":"z"}}}`,
			`[{"k": "a", "x": 1, "y": "z"}, {"k": "b", "x": 0}]`},
		{"increment beyond int64", `[{"k":"a","x":99999999999999999999}]`,
			`{"n":1,"i":{"0":{"x":2}}}`, `[{"k": "a", "x": 100000000000000000001}]`},
		{"global increment on integers only",
			`[{"x":1},{"x":"7"},{"x":1.5},{"x":null},{"y":0},{"x":-3}]`,
			`{"n":6,"g":{"x":10},"i":{"5":{"x":1}}}`,
			`[{"x": 11}, {"x": "7"}, {"x": 1.5}, {"x": null}, {"y": 0}, {"x": 8}]`},
		{"global, increment, then value", `[{"x":1},{"x":2}]`,
			`{"n":2,"g":{"x":10},"i":{"0":{"x":-10}},"u":{"1":{"x":"done"}}}`,
			`[{"x": 1}, {"x": "done"}]`},
		{"remove and append", base, `{"n":2,"d":[0],"a":[{"k":"c"}]}`,
			`[{"k": "b", "x": 2}, {"k": "c"}]`},
		{"explicit order", base, `{"n":2,"a":[{"k":"c"}],"o":[2,0]}`,
			`[{"k": "c"}, {"k": "a", "x": 1}]`},
		{"order with values", base, `{"n":2,"u":{"0":{"x":9}},"o":[1,0]}`,
			`[{"k": "b", "x": 2}, {"k": "a", "x": 9}]`},
		{"empty result", base, `{"n":0,"d":[0,1]}`, `[]`},
		{"empty base", `[]`, `{"n":1,"a":[{"k":"z"}]}`, `[{"k": "z"}]`},
		{"non-list base", `{"k":"a"}`, `{"n":0}`, "<NULL>"},
		{"object document", `{"a":1,"b":[1,2],"c":5}`,
			`{"w":true,"n":1,"g":{"c":2},"u":{"0":{"a":"x"}}}`,
			`{"a": "x", "b": [1, 2], "c": 7}`},
		{"object delta on a list", `[{"a":1}]`, `{"w":true,"n":1}`, "<NULL>"},
	}
	for _, tc := range cases {
		var got *string // NULL is spelled "<NULL>"
		err := pool.QueryRow(ctx, `SELECT sage.snapshot_apply($1::jsonb, $2::jsonb)::text`,
			tc.base, tc.delta).Scan(&got)
		if err != nil || deref(got) != tc.want {
			t.Errorf("%s: got %s (%v)\n want %s", tc.name, deref(got), err, tc.want)
		}
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT sage.snapshot_apply(NULL, '{"n":0}') IS NULL`).
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
		sage.snapshot_data('{"n":1,"u":{"0":{"x":2}}}', $1)::text,
		sage.snapshot_data('{"n":1}', -1)::text`, baseID).
		Scan(&full, &delta, &orphan)
	if err != nil {
		t.Fatalf("snapshot_data: %v", err)
	}
	if full == nil || *full != "[1]" || delta == nil || *delta != `[{"k": "a", "x": 2}]` ||
		orphan != nil {
		t.Fatalf("full=%v delta=%v orphan=%v", deref(full), deref(delta), deref(orphan))
	}
}

// A delta on a checkpoint (itself a delta on the keyframe) applies both,
// oldest first; a chain deeper than any the writer makes reads as NULL.
func TestSnapshotData_ChainedBases(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sage.snapshots WHERE category = 'chain_test'`)
	})
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now(), 'chain_test', '[{"k":"a","x":1}]') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert keyframe: %v", err)
	}
	for depth := 1; depth <= 9; depth++ {
		if err := pool.QueryRow(ctx, `INSERT INTO sage.snapshots
			(collected_at, category, data, base_id)
			VALUES (now(), 'chain_test', '{"n":1,"i":{"0":{"x":1}}}', $1)
			RETURNING id`, id).Scan(&id); err != nil {
			t.Fatalf("insert depth %d: %v", depth, err)
		}
		var got *string
		if err := pool.QueryRow(ctx, `SELECT sage.snapshot_data(data, base_id)::text
			FROM sage.snapshots WHERE id = $1`, id).Scan(&got); err != nil {
			t.Fatalf("read depth %d: %v", depth, err)
		}
		want := `[{"k": "a", "x": ` + strconv.Itoa(1+depth) + `}]`
		if depth > 8 && got != nil {
			t.Fatalf("depth %d = %s, want NULL beyond the chain limit", depth, *got)
		}
		if depth <= 8 && (got == nil || *got != want) {
			t.Fatalf("depth %d = %s, want %s", depth, deref(got), want)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return *s
}
