package main

import (
	"strings"
	"testing"
)

// No concurrent access tests: newInstallLog is a pure function.

// The new-install line describes the level it names: the read-only
// explanation belongs to observation only.
func TestNewInstallLogObservationExplainsReadOnly(t *testing.T) {
	got := newInstallLog("mydb", "observation")
	if !strings.Contains(got, `db "mydb": new install, trust observation`) ||
		!strings.Contains(got, "nothing changes outside the sage schema") {
		t.Fatalf("got %q", got)
	}
}

func TestNewInstallLogOtherLevelsDoNotClaimObservation(t *testing.T) {
	for _, level := range []string{"advisory", "autonomous"} {
		got := newInstallLog("mydb", level)
		if !strings.HasPrefix(got, `db "mydb": new install, trust `+level) ||
			strings.Contains(got, "observation") ||
			strings.Contains(got, "nothing changes outside the sage schema") {
			t.Fatalf("level %q: got %q", level, got)
		}
	}
}
