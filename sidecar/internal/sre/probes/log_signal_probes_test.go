package probes

import (
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// No concurrent access or integration tests: signalProbes is a read-only
// package map.

// rca-07 / CHECK-03: the log incidents that had no investigation probe get
// the catalog probes that explain them.
func TestForSignal_LogIncidentsHaveProbes(t *testing.T) {
	want := map[string][]ID{
		signalid.LogDeadlockDetected:      {LockGraph, LockChains},
		signalid.LogConnectionRefused:     {ConnectionSaturation},
		signalid.LogArchiveFailed:         {Archiver},
		signalid.LogTxidWraparoundWarning: {AutovacuumWraparound},
		signalid.LogWALSegmentRemoved:     {ReplicationSlots},
	}
	for sig, ids := range want {
		got := ForSignal(sig)
		if !reflect.DeepEqual(got, ids) {
			t.Errorf("ForSignal(%s) = %v, want %v", sig, got, ids)
		}
	}
}

// Invalid input: the info-only cancel signal opens no incident, so it has
// no probes.
func TestForSignal_StatementCancelledHasNoProbes(t *testing.T) {
	if got := ForSignal(signalid.LogStatementCancelled); got != nil {
		t.Fatalf("ForSignal(log_statement_cancelled) = %v, want nil", got)
	}
}
