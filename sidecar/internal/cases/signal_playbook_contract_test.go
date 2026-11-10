package cases

import (
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// No concurrent access tests: the table is a package-level read-only map.
// No integration tests here: rca/playbook_bridge_db_test.go projects a case
// from an incident the engine wrote to real PostgreSQL.

// wantSignalPlaybooks is the deliberate decision for every signal id an
// incident can carry (playbooks-02). Adding a signal to signalid without
// adding it here fails TestEverySignalHasADeliberatePlaybook.
var wantSignalPlaybooks = map[string]incidentPlaybook{
	signalid.ConnectionsHigh:          playbookConnections,
	signalid.IdleInTxElevated:         playbookLock,
	signalid.CacheHitRatioDrop:        playbookNone,
	signalid.ReplicationLagIncreasing: playbookWAL,
	signalid.VacuumBlocked:            playbookVacuumBlocked,
	signalid.LockContention:           playbookLock,
	signalid.WALGrowthSpike:           playbookWAL,
	signalid.OrphanedPreparedTx:       playbookFreeze,

	signalid.LogDeadlockDetected:           playbookNone,
	signalid.LogConnectionRefused:          playbookConnections,
	signalid.LogOutOfMemory:                playbookNone,
	signalid.LogDiskFull:                   playbookNone,
	signalid.LogPanicServerCrash:           playbookNone,
	signalid.LogDataCorruption:             playbookNone,
	signalid.LogTxidWraparoundWarning:      playbookFreeze,
	signalid.LogArchiveFailed:              playbookNone,
	signalid.LogTempFileCreated:            playbookNone,
	signalid.LogCheckpointTooFrequent:      playbookNone,
	signalid.LogLockTimeout:                playbookNone,
	signalid.LogStatementTimeout:           playbookRunaway,
	signalid.LogReplicationConflict:        playbookStandby,
	signalid.LogWALSegmentRemoved:          playbookWAL,
	signalid.LogAutovacuumCancel:           playbookVacuumPressure,
	signalid.LogReplicationSlotInactive:    playbookWAL,
	signalid.LogAuthenticationFailure:      playbookNone,
	signalid.LogSlowQuery:                  playbookRunaway,
	signalid.LogInternalError:              playbookNone,
	signalid.LogWraparoundOutage:           playbookFreeze,
	signalid.LogMultiXactWraparoundWarning: playbookFreeze,
	signalid.LogMultiXactWraparoundOutage:  playbookFreeze,

	signalid.SRECheckpointStorm:   playbookNone,
	signalid.SRETempFileExplosion: playbookNone,
	signalid.SRELWLockContention:  playbookNone,
}

// CHECK-02: every emitted id (33: 8 snapshot, 22 log, 3 detector) has an
// explicit entry, "none" included.
func TestEverySignalHasADeliberatePlaybook(t *testing.T) {
	all := signalid.All()
	if len(all) != 33 {
		t.Fatalf("signalid.All() = %d ids, want 33", len(all))
	}
	for _, info := range all {
		got, ok := signalPlaybooks[info.ID]
		if !ok {
			t.Errorf("signal %q (%s) has no playbook decision", info.ID, info.Family)
			continue
		}
		if want := wantSignalPlaybooks[info.ID]; got != want {
			t.Errorf("signal %q -> %q, want %q", info.ID, got, want)
		}
	}
}

// Invalid input: the table holds no id the writers never emit.
func TestPlaybookTableHasOnlyEmittedSignals(t *testing.T) {
	emitted := map[string]bool{}
	for _, info := range signalid.All() {
		emitted[info.ID] = true
	}
	var extra []string
	for id := range signalPlaybooks {
		if !emitted[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Fatalf("playbook table keys no writer emits: %v", extra)
	}
	if len(signalPlaybooks) != len(emitted) {
		t.Fatalf("table has %d entries, want %d", len(signalPlaybooks), len(emitted))
	}
}

// Invalid input: ids invented by the old selectors resolve to "none".
func TestInventedSignalIDsResolveToNone(t *testing.T) {
	for _, id := range []string{
		"runaway_query", "long_running_query", "replication_lag",
		"inactive_slot", "standby_conflict", "replica_conflict",
		"sequence_exhaustion", "autovacuum_falling_behind", "autovacuum_blocked",
		"vacuum_falling_behind", "wal_archive_failed", "", "CONNECTIONS_HIGH",
	} {
		if got := playbookForSignals([]string{id}); got != playbookNone {
			t.Errorf("invented id %q -> %q, want none", id, got)
		}
	}
}

// Nil/empty: no signals resolve to none.
func TestNoSignalsResolveToNone(t *testing.T) {
	if got := playbookForSignals(nil); got != playbookNone {
		t.Fatalf("nil signals -> %q", got)
	}
	if got := playbookForSignals([]string{}); got != playbookNone {
		t.Fatalf("empty signals -> %q", got)
	}
}

// Boundary: with several mapped signals the precedence is fixed and does
// not depend on the order RCA listed them.
func TestPlaybookPrecedenceIsOrderIndependent(t *testing.T) {
	for _, test := range []struct {
		ids  []string
		want incidentPlaybook
	}{
		{[]string{signalid.WALGrowthSpike, signalid.LogSlowQuery}, playbookRunaway},
		{[]string{signalid.LogSlowQuery, signalid.WALGrowthSpike}, playbookRunaway},
		{[]string{signalid.ReplicationLagIncreasing, signalid.LogReplicationConflict},
			playbookStandby},
		{[]string{signalid.LogReplicationConflict, signalid.ReplicationLagIncreasing},
			playbookStandby},
		{[]string{signalid.LogOutOfMemory, signalid.LogTxidWraparoundWarning},
			playbookFreeze},
		{[]string{"unknown", signalid.WALGrowthSpike}, playbookWAL},
	} {
		if got := playbookForSignals(test.ids); got != test.want {
			t.Errorf("%v -> %q, want %q", test.ids, got, test.want)
		}
	}
}
