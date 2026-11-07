//go:build !cgo

package sqlast

import (
	"errors"
	"testing"
)

func TestInspectStatementsFailsClosedWithoutCgo(t *testing.T) {
	got, err := InspectStatements("ALTER TABLE orders ADD COLUMN x int")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if got != nil {
		t.Fatalf("statements = %v, want nil", got)
	}
}
