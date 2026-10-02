package causal

import "testing"

// Graph v3 (Sage SRE M6) adds four reactive families. Amplifiers only
// ever contribute: a write burst amplifies requested checkpoints, a
// primary write surge amplifies every replication stage, and a replay
// backlog on a standby is the symptom of a paused replay or of standby
// queries holding replay back.

func amplifies(t *testing.T, id NodeID, want ...NodeID) {
	t.Helper()
	n, ok := NodeByID(id)
	if !ok {
		t.Fatalf("graph lacks %s", id)
	}
	if len(n.Amplifies) != len(want) {
		t.Fatalf("%s amplifies %v, want %v", id, n.Amplifies, want)
	}
	for i, a := range want {
		if n.Amplifies[i] != a {
			t.Fatalf("%s amplifies %v, want %v", id, n.Amplifies, want)
		}
	}
}

func TestGraphV3_Amplifiers(t *testing.T) {
	if GraphVersion != "causal-v4" {
		t.Fatalf("graph version %s, want causal-v4 (M6 families)", GraphVersion)
	}
	amplifies(t, CheckpointWriteBurst, MaxWALSizeUndersized, ForcedCheckpoints)
	amplifies(t, ReplicationWriteSurge, WALSendBacklog, StandbyFlushBacklog,
		StandbyReplayBacklog)
	amplifies(t, StandbyReplayBacklog, ReplayPaused, StandbyQueryDelay)
	for _, id := range []NodeID{MaxWALSizeUndersized, ForcedCheckpoints,
		ShortCheckpointTimeout, RunawaySpillQuery, RepeatedSpillStatement,
		WorkMemUndersized, WALSendBacklog, StandbyFlushBacklog, ReplayPaused,
		StandbyQueryDelay, LockManagerContention, SubtransSLRUContention,
		MultiXactSLRUContention, WALWriteContention, BufferContention} {
		amplifies(t, id)
	}
}

func TestGraphV3_FamiliesHaveCompetingHypotheses(t *testing.T) {
	count := map[Family]int{}
	for _, n := range Graph() {
		count[n.Family]++
	}
	want := map[Family]int{FamilyCheckpoint: 4, FamilyTempFiles: 3,
		FamilyReplicationLag: 6, FamilyLWLock: 5}
	for f, n := range want {
		if count[f] != n {
			t.Errorf("family %s has %d nodes, want %d", f, count[f], n)
		}
	}
}

// Operator steps never recommend the dangerous "fixes" of the taxonomy:
// fleet-wide work_mem increases, fsync/full_page_writes off, restarts as
// the fix, or failing over to a lagging replica.
func TestGraphV3_OperatorStepsAvoidDangerousFixes(t *testing.T) {
	bad := []string{"fsync=off", "fsync = off", "full_page_writes=off",
		"full_page_writes = off", "restart the server", "fail over",
		"raise work_mem globally", "drop the slot"}
	for _, n := range Graph() {
		if n.Family != FamilyCheckpoint && n.Family != FamilyTempFiles &&
			n.Family != FamilyReplicationLag && n.Family != FamilyLWLock {
			continue
		}
		for _, b := range bad {
			if containsFold(n.OperatorStep, b) {
				t.Errorf("%s operator step suggests %q: %s", n.ID, b, n.OperatorStep)
			}
		}
	}
}
