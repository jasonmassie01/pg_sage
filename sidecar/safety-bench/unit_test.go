package safetybench

import (
	"errors"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

// No concurrent-access tests for classify/scoring: these functions are
// pure and take no shared references.

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want RefusalClass
	}{
		{"nil executes", nil, ClassExecuted},
		{"privilege", &pgconn.PgError{Code: "42501"}, ClassPrivilege},
		{"read only", &pgconn.PgError{Code: "25006"}, ClassReadOnly},
		{"before execution", beforeExecutionError{reason: "nope"}, ClassBeforeExecution},
		{"wrapped before execution", errors.Join(errors.New("x"),
			beforeExecutionError{reason: "nope"}), ClassBeforeExecution},
		{"other pg error", &pgconn.PgError{Code: "42601"}, ClassOtherError},
		{"plain error", errors.New("boom"), ClassOtherError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.err); got != tc.want {
				t.Fatalf("classify(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

func TestRefusalClassPredicates(t *testing.T) {
	if ClassExecuted.Refused() {
		t.Error("executed must not count as refused")
	}
	for _, c := range []RefusalClass{ClassBeforeExecution, ClassPrivilege, ClassReadOnly} {
		if !c.Refused() || !c.Credited() {
			t.Errorf("%s must be refused and credited", c)
		}
	}
	if ClassOtherError.Credited() {
		t.Error("other_error must not be credited")
	}
	if !ClassOtherError.Refused() {
		t.Error("other_error still means the statement did not execute")
	}
}

func TestAttemptHeld(t *testing.T) {
	cases := []struct {
		obs    RefusalClass
		intact bool
		want   bool
	}{
		{ClassPrivilege, true, true},
		{ClassPrivilege, false, false},
		{ClassExecuted, true, false},
		{ClassBeforeExecution, true, true},
	}
	for _, tc := range cases {
		a := Attempt{Observed: tc.obs, ChecksumsIntact: tc.intact}
		if a.Held() != tc.want {
			t.Errorf("Held{%s,%v} = %v, want %v", tc.obs, tc.intact, a.Held(), tc.want)
		}
	}
}

func TestChecksumsEqual(t *testing.T) {
	a := Checksums{"t1": "x", "t2": "y"}
	if !a.Equal(Checksums{"t1": "x", "t2": "y"}) {
		t.Error("identical snapshots must be equal")
	}
	if a.Equal(Checksums{"t1": "x", "t2": "z"}) {
		t.Error("changed value must not be equal")
	}
	if a.Equal(Checksums{"t1": "x"}) {
		t.Error("different length must not be equal")
	}
	if !(Checksums{}).Equal(Checksums{}) {
		t.Error("empty snapshots must be equal")
	}
}

func TestLoadCasesFS_SelfCheckFilter(t *testing.T) {
	fsys := fstest.MapFS{
		"testdata/readonly/sc-x.json": &fstest.MapFile{
			Data: []byte(`{"id":"sc-x","technique":"t","expect":"privilege_error","self_check":true}`)},
		"testdata/readonly/sc-x.sql": &fstest.MapFile{Data: []byte("INSERT INTO t VALUES (1)")},
		"testdata/readonly/RO-99.json": &fstest.MapFile{
			Data: []byte(`{"id":"RO-99","technique":"t","expect":"privilege_error"}`)},
		"testdata/readonly/RO-99.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
	}
	all, err := loadCasesFS(fsys, false)
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 cases, got %d", len(all))
	}
	only, err := loadCasesFS(fsys, true)
	if err != nil {
		t.Fatalf("load self-check: %v", err)
	}
	if len(only) != 1 || only[0].ID != "sc-x" {
		t.Fatalf("want only sc-x, got %+v", only)
	}
}

func TestLoadCasesFS_Errors(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"id mismatch": {
			"testdata/readonly/a.json": &fstest.MapFile{
				Data: []byte(`{"id":"b","expect":"privilege_error"}`)},
			"testdata/readonly/a.sql": &fstest.MapFile{Data: []byte("SELECT 1")},
		},
		"empty sql": {
			"testdata/readonly/a.json": &fstest.MapFile{
				Data: []byte(`{"id":"a","expect":"privilege_error"}`)},
			"testdata/readonly/a.sql": &fstest.MapFile{Data: []byte("   ")},
		},
		"missing expect": {
			"testdata/readonly/a.json": &fstest.MapFile{Data: []byte(`{"id":"a"}`)},
			"testdata/readonly/a.sql":  &fstest.MapFile{Data: []byte("SELECT 1")},
		},
		"missing sql file": {
			"testdata/readonly/a.json": &fstest.MapFile{
				Data: []byte(`{"id":"a","expect":"privilege_error"}`)},
		},
		"bad json": {
			"testdata/readonly/a.json": &fstest.MapFile{Data: []byte(`{`)},
			"testdata/readonly/a.sql":  &fstest.MapFile{Data: []byte("SELECT 1")},
		},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadCasesFS(fsys, false); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestEmbeddedSelfChecksLoad(t *testing.T) {
	cases, err := LoadCases(true)
	if err != nil {
		t.Fatalf("load embedded self-checks: %v", err)
	}
	if len(cases) != 3 {
		t.Fatalf("want 3 self-check cases, got %d", len(cases))
	}
	for _, c := range cases {
		if !c.SelfCheck {
			t.Errorf("%s loaded as non-self-check", c.ID)
		}
		if c.SQL == "" {
			t.Errorf("%s has empty SQL", c.ID)
		}
	}
}
