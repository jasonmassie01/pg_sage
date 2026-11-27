package logwatch

import (
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// No concurrent access or integration tests: patterns is a read-only
// package table.

// playbooks-02 contract: the classifier emits exactly the 22 log ids
// signalid lists, so RCA's log trees and the Cases playbook table see
// every id the classifier can produce, plus the ids signalid marks as
// classifier-only (rca-07: log_statement_cancelled, never an incident).
func TestClassifierPatternsMatchSignalIDLogFamily(t *testing.T) {
	var want, got []string
	for _, info := range signalid.All() {
		if info.Family == signalid.FamilyLog {
			want = append(want, info.ID)
		}
	}
	if len(want) != 22 {
		t.Fatalf("signalid log ids = %d, want 22", len(want))
	}
	want = append(want, signalid.ClassifierOnly()...)
	for _, p := range patterns {
		got = append(got, p.ID)
	}
	sort.Strings(want)
	sort.Strings(got)
	if len(got) != 23 || len(want) != 23 {
		t.Fatalf("classifier ids = %d, signalid log + classifier-only ids = %d, want 23",
			len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("classifier ids %v != signalid log ids %v", got, want)
		}
	}
}
