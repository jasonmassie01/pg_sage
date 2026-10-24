//go:build !cgo

package receipt

import (
	"strings"
	"testing"
)

// Without libpg_query no statement can be shown to be allowlisted, so no
// review is eligible.
func TestEligibleWithoutTheParser(t *testing.T) {
	r := shadowReview(t, cic, gib, "table")
	if ok, why := Eligible(r, 10*gib); ok || !strings.Contains(why, "parser unavailable") {
		t.Fatalf("Eligible = %v (%q), want false: parser unavailable", ok, why)
	}
}
