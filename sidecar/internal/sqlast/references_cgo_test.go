//go:build cgo

package sqlast

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
)

type refsEntry struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
	Want Refs   `json:"want"`
}

func loadRefsCorpus(t *testing.T) []refsEntry {
	t.Helper()
	raw, err := os.ReadFile("testdata/references_corpus.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus []refsEntry
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	return corpus
}

// CHECK-SQL-03: relations, (relation, column) pairs, star relations,
// ambiguous columns, statement kind and implied lock mode per fixture.
func TestReferencesCorpus(t *testing.T) {
	corpus := loadRefsCorpus(t)
	if len(corpus) < 30 {
		t.Fatalf("corpus has %d queries, want at least 30", len(corpus))
	}
	for _, entry := range corpus {
		t.Run(entry.Name, func(t *testing.T) {
			got, err := References(entry.SQL)
			if err != nil {
				t.Fatalf("References(%q) error = %v", entry.SQL, err)
			}
			if !reflect.DeepEqual(got, entry.Want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(entry.Want)
				t.Errorf("References(%q)\n got  %s\n want %s", entry.SQL, g, w)
			}
		})
	}
}

// pg_stat_statements text carries $n parameters and may end in a
// semicolon; both parse.
func TestReferencesOnStatementsText(t *testing.T) {
	got, err := References("SELECT email FROM rv.orders WHERE id = $1 AND status = $2;")
	if err != nil {
		t.Fatalf("References error = %v", err)
	}
	want := []ColumnPair{
		{QualifiedName{"rv", "orders"}, "email"},
		{QualifiedName{"rv", "orders"}, "id"},
		{QualifiedName{"rv", "orders"}, "status"},
	}
	if !reflect.DeepEqual(got.Columns, want) || got.Kind != StmtSelect {
		t.Errorf("got %+v, want columns %+v", got, want)
	}
}

func TestReferencesRejects(t *testing.T) {
	for _, sql := range []string{
		"", "   ", "SELECT 1; SELECT 2", "SELEC 1", "<insufficient privilege>",
	} {
		got, err := References(sql)
		if !errors.Is(err, ErrRejected) || !reflect.DeepEqual(got, Refs{}) {
			t.Errorf("References(%q) = %+v, %v; want zero, ErrRejected", sql, got, err)
		}
	}
}

func TestStrongestLockMode(t *testing.T) {
	cases := []struct {
		sql  string
		want LockMode
	}{
		{"SELECT 1 FROM rv.a", LockAccessShare},
		{"SELECT 1 FROM rv.a FOR NO KEY UPDATE", LockRowShare},
		{"WITH u AS (UPDATE rv.b SET x = 1 RETURNING x) SELECT * FROM rv.a, u", LockRowExclusive},
		{"SELECT 1 FROM rv.a WHERE EXISTS (SELECT 1 FROM rv.b FOR UPDATE)", LockRowShare},
		{"DELETE FROM rv.a", LockRowExclusive},
	}
	for _, c := range cases {
		got, err := References(c.sql)
		if err != nil || got.LockMode != c.want {
			t.Errorf("References(%q) lock = %q, %v; want %q", c.sql, got.LockMode, err, c.want)
		}
	}
}
