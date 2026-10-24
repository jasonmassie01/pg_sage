//go:build cgo

package sqlast

import (
	"errors"
	"sort"
	"testing"
)

var fuzzSeeds = []string{
	"", ";", "BEGIN; COMMIT", "SET lock_timeout = '2s'", "SELECT 'é' AS \"ü\"",
	"CREATE INDEX CONCURRENTLY i ON t (lower(a)) WHERE b IS NULL",
	"ALTER TABLE t ADD COLUMN a int DEFAULT nextval('s'), DROP COLUMN b CASCADE",
	"WITH RECURSIVE r AS (SELECT 1 UNION SELECT r.x FROM r) SELECT * FROM r",
	"UPDATE t SET a = b.c FROM b WHERE t.id = b.id RETURNING *",
	"MERGE INTO t USING s ON t.id = s.id WHEN NOT MATCHED THEN INSERT VALUES (s.id)",
	"/* unterminated", "SELECT $1, $2 FROM \"Q\".\"T\" x(a, b) NATURAL JOIN y",
	"CREATE TABLE p (a int) PARTITION BY HASH (a)", "DROP INDEX a, b, c",
}

// FuzzInspectDDL: no input panics; a result always slices the input
// exactly, and every failure is an ErrRejected.
func FuzzInspectDDL(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		stmts, err := InspectDDL(sql)
		if err != nil {
			if !errors.Is(err, ErrRejected) || stmts != nil {
				t.Fatalf("InspectDDL(%q) = %v, %v; want nil, ErrRejected", sql, stmts, err)
			}
			return
		}
		for _, s := range stmts {
			if s.Offset < 0 || s.Offset+len(s.Text) > len(sql) ||
				sql[s.Offset:s.Offset+len(s.Text)] != s.Text {
				t.Fatalf("InspectDDL(%q): Text %q not at offset %d", sql, s.Text, s.Offset)
			}
			if s.Kind == "" || s.Command == "" || s.Line < 1 {
				t.Fatalf("InspectDDL(%q): incomplete entry %+v", sql, s)
			}
		}
	})
}

// FuzzReferences: no input panics; results are sorted and de-duplicated,
// and every failure is an ErrRejected.
func FuzzReferences(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		refs, err := References(sql)
		if err != nil {
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("References(%q) error = %v, want ErrRejected", sql, err)
			}
			return
		}
		if refs.Kind == "" {
			t.Fatalf("References(%q): empty kind", sql)
		}
		if !sort.SliceIsSorted(refs.Relations, func(i, j int) bool {
			a, b := refs.Relations[i], refs.Relations[j]
			return a.Schema < b.Schema || (a.Schema == b.Schema && a.Name < b.Name)
		}) {
			t.Fatalf("References(%q): relations not sorted: %v", sql, refs.Relations)
		}
		seen := map[ColumnPair]bool{}
		for _, c := range refs.Columns {
			if seen[c] {
				t.Fatalf("References(%q): duplicate column %v", sql, c)
			}
			seen[c] = true
		}
	})
}
