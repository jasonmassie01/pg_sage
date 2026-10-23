//go:build !cgo

package sqlast

import (
	"errors"
	"testing"
)

// Without the parser a brokered read cannot be verified, so it is refused
// (spec §6.8 S2: verified fail-closed).
func TestInspectBrokeredReadFailsClosedWithoutCgo(t *testing.T) {
	got, err := InspectBrokeredRead("SELECT 1")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if got.Canonical != "" || got.Fingerprint != "" {
		t.Errorf("result = %+v, want empty", got)
	}
}
