package sre

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// No concurrent access or integration tests: both tables are read-only
// package maps; triggers_db_test.go reads them against real PostgreSQL.

func emittedSignals() map[string]signalid.Family {
	out := map[string]signalid.Family{}
	for _, info := range signalid.All() {
		out[info.ID] = info.Family
	}
	return out
}

// playbooks-02 contract: every incident signal that starts an
// investigation is one RCA or the detector actually writes.
func TestTriggerSignalsAreEmittedIDs(t *testing.T) {
	emitted := emittedSignals()
	if len(signalKinds) == 0 {
		t.Fatal("signalKinds is empty")
	}
	for id, kind := range signalKinds {
		if _, ok := emitted[id]; !ok {
			t.Errorf("trigger signal %q (%s) is not in signalid.All()", id, kind)
		}
	}
}

func TestDetectorSignalsAreDetectorFamily(t *testing.T) {
	emitted := emittedSignals()
	if len(detectorSignals) != 3 {
		t.Fatalf("detectorSignals = %d, want 3", len(detectorSignals))
	}
	for kind, id := range detectorSignals {
		if emitted[id] != signalid.FamilyDetector {
			t.Errorf("detector %s writes %q, family %q, want detector", kind, id,
				emitted[id])
		}
		// A detector incident resumes its own family after a restart.
		if signalKinds[id] != kind {
			t.Errorf("detector signal %q maps back to %q, want %q", id,
				signalKinds[id], kind)
		}
	}
}
