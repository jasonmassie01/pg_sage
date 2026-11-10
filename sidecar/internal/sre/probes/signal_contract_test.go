package probes

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// No concurrent access or integration tests: signalProbes is a read-only
// package map; the probes themselves are covered by catalog_db_test.go.

// playbooks-02 contract: every signal with an investigation probe set is
// one RCA or the detector actually writes, and every probe it names exists.
func TestSignalProbeKeysAreEmittedIDs(t *testing.T) {
	emitted := map[string]bool{}
	for _, info := range signalid.All() {
		emitted[info.ID] = true
	}
	if len(signalProbes) == 0 {
		t.Fatal("signalProbes is empty")
	}
	for id, ids := range signalProbes {
		if !emitted[id] {
			t.Errorf("probe signal %q is not in signalid.All()", id)
		}
		for _, probe := range ids {
			if _, ok := Catalog().Spec(probe); !ok {
				t.Errorf("signal %q names unknown probe %q", id, probe)
			}
		}
	}
	if got := ForSignal("replica_lag"); got != nil {
		t.Fatalf("invented id has probes: %v", got)
	}
}
