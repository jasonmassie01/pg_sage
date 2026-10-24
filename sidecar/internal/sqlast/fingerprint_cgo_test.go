//go:build cgo

package sqlast

import (
	"errors"
	"regexp"
	"testing"
)

func mustFingerprint(t *testing.T, sql string) string {
	t.Helper()
	fp, err := Fingerprint(sql)
	if err != nil {
		t.Fatalf("Fingerprint(%q) error = %v", sql, err)
	}
	return fp
}

// CHECK-SQL-04 (parser half): literals, $n parameters, comments, case and
// whitespace do not change the fingerprint; the predicate column does.
func TestFingerprintMatchesNormalizedForms(t *testing.T) {
	base := mustFingerprint(t, "SELECT * FROM s.t WHERE id = $1")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(base) {
		t.Fatalf("fingerprint %q is not 16 hex digits", base)
	}
	for _, same := range []string{
		"SELECT * FROM s.t WHERE id = 42 /* app='x' */",
		"select *\n  from s.t\n where id = 7",
		"SELECT * FROM s.t WHERE id = $2;",
		"-- lead comment\nSELECT * FROM s.t WHERE id = 'abc'",
	} {
		if got := mustFingerprint(t, same); got != base {
			t.Errorf("Fingerprint(%q) = %s, want %s", same, got, base)
		}
	}
	for _, other := range []string{
		"SELECT * FROM s.t WHERE code = $1",
		"SELECT * FROM s.u WHERE id = $1",
		"SELECT id FROM s.t WHERE id = $1",
		"SELECT * FROM s.t WHERE id > $1",
	} {
		if got := mustFingerprint(t, other); got == base {
			t.Errorf("Fingerprint(%q) = %s equals the id = $1 fingerprint", other, got)
		}
	}
}

func TestFingerprintDDLIgnoresFormatting(t *testing.T) {
	a := mustFingerprint(t, "ALTER TABLE rv.orders ADD COLUMN note text")
	b := mustFingerprint(t, "alter table rv.orders\n  add column note text; -- why")
	c := mustFingerprint(t, "ALTER TABLE rv.orders ADD COLUMN memo text")
	if a != b {
		t.Errorf("formatting changed the DDL fingerprint: %s vs %s", a, b)
	}
	if a == c {
		t.Errorf("a different column kept the fingerprint %s", a)
	}
}

func TestFingerprintRejectsEmptyAndInvalid(t *testing.T) {
	for _, sql := range []string{"", "  ", "-- nothing", "SELEC 1", "<insufficient privilege>"} {
		fp, err := Fingerprint(sql)
		if fp != "" || !errors.Is(err, ErrRejected) {
			t.Errorf("Fingerprint(%q) = %q, %v; want ErrRejected", sql, fp, err)
		}
	}
}
