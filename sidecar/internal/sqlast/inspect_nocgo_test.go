//go:build !cgo

package sqlast

import (
	"errors"
	"testing"
)

// CHECK-SQL-06: a build without cgo has no parser, so every change-review
// inspection fails closed with ErrUnavailable instead of a partial answer.
func TestInspectionsUnavailableWithoutCgo(t *testing.T) {
	if Available() {
		t.Fatal("Available() = true in a build without cgo")
	}
	const sql = "ALTER TABLE rv.orders ADD COLUMN note text"
	stmts, err := InspectDDL(sql)
	if !errors.Is(err, ErrUnavailable) || stmts != nil {
		t.Errorf("InspectDDL = %v, %v; want nil, ErrUnavailable", stmts, err)
	}
	refs, err := References("SELECT email FROM rv.orders")
	if !errors.Is(err, ErrUnavailable) || refs.Kind != "" || refs.Relations != nil {
		t.Errorf("References = %+v, %v; want zero, ErrUnavailable", refs, err)
	}
	fp, err := Fingerprint("SELECT 1")
	if !errors.Is(err, ErrUnavailable) || fp != "" {
		t.Errorf("Fingerprint = %q, %v; want empty, ErrUnavailable", fp, err)
	}
	shape, err := ReadShape("DELETE FROM rv.orders WHERE id = 1")
	if !errors.Is(err, ErrUnavailable) || shape != "" {
		t.Errorf("ReadShape = %q, %v; want empty, ErrUnavailable", shape, err)
	}
	// Even input the cgo build would reject reports ErrUnavailable, never
	// ErrRejected: callers must not read "rejected" as "parsed and refused".
	for _, sql := range []string{"", "SELEC 1"} {
		if _, err := InspectDDL(sql); !errors.Is(err, ErrUnavailable) ||
			errors.Is(err, ErrRejected) {
			t.Errorf("InspectDDL(%q) error = %v, want only ErrUnavailable", sql, err)
		}
	}
}
