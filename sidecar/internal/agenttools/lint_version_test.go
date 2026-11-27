package agenttools

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: normalizeServerVersion is a pure function.

// Boundary (migration-12): a bare major from 14 to 18 is scaled to
// server_version_num; server_version_num from 140000 to 189999 passes.
func TestNormalizeServerVersionAcceptsSupportedForms(t *testing.T) {
	cases := map[int]int{
		14: 140000, 15: 150000, 18: 180000,
		140000: 140000, 170004: 170004, 189999: 189999,
	}
	for in, want := range cases {
		got, err := normalizeServerVersion(in)
		if err != nil || got != want {
			t.Errorf("normalizeServerVersion(%d) = %d, %v; want %d", in, got, err, want)
		}
	}
}

// Invalid input: anything outside the two accepted forms is an invalid
// request whose message names both forms.
func TestNormalizeServerVersionRejectsUnsupported(t *testing.T) {
	for _, in := range []int{
		-1, 1, 9, 13, 19, 100, 139999, 130000, 190000, 1999999,
	} {
		got, err := normalizeServerVersion(in)
		if err == nil {
			t.Errorf("normalizeServerVersion(%d) = %d, want an error", in, got)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("normalizeServerVersion(%d) error %v is not ErrInvalid", in, err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "14 to 18") || !strings.Contains(msg, "140000 to 189999") {
			t.Errorf("normalizeServerVersion(%d) error %q does not name the accepted forms",
				in, msg)
		}
	}
}

// Nil/zero: 0 is not normalized here; it means "read the server's".
func TestNormalizeServerVersionZeroIsNotAVersion(t *testing.T) {
	if _, err := normalizeServerVersion(0); err == nil {
		t.Fatal("normalizeServerVersion(0) accepted 0; the caller must read the server")
	}
}

// Boundary on real PostgreSQL: 14, 140000 and 170004 give identical
// items; 13, 130000 and 1999999 are invalid requests.
func TestLintMigrationVersionFormsAgree(t *testing.T) {
	f := newFixture(t)
	table := lintTable(f)
	sql := "VACUUM FULL " + table + "; ALTER TABLE " + table +
		" ADD COLUMN d text DEFAULT 'x'; REINDEX TABLE " + table
	tools := New(f.pool, Options{})
	var first []LintItem
	for i, v := range []int{14, 140000, 170004, 0} {
		res, err := tools.LintMigration(f.ctx, LintRequest{SQL: sql, PGVersion: v})
		require.NoError(t, err)
		require.NotEmpty(t, res.Statements)
		if i == 0 {
			first = res.Statements
			continue
		}
		requireSameRules(t, first, res.Statements, v)
	}
	for _, v := range []int{13, 130000, 1999999} {
		_, err := tools.LintMigration(f.ctx, LintRequest{SQL: sql, PGVersion: v})
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("pg_version %d: err = %v, want ErrInvalid", v, err)
		}
	}
}

// requireSameRules compares the version-dependent part of two lint runs:
// rule, lock, rewrite flag and safe alternative (scores read live stats).
func requireSameRules(t *testing.T, want, got []LintItem, version int) {
	t.Helper()
	strip := func(items []LintItem) []LintItem {
		out := make([]LintItem, len(items))
		for i, it := range items {
			out[i] = LintItem{Statement: it.Statement, RuleID: it.RuleID,
				LockLevel: it.LockLevel, RequiresRewrite: it.RequiresRewrite,
				Table: it.Table, SafeAlternative: it.SafeAlternative}
		}
		return out
	}
	if !reflect.DeepEqual(strip(want), strip(got)) {
		t.Fatalf("pg_version %d items differ:\n got %+v\nwant %+v", version, got, want)
	}
}
