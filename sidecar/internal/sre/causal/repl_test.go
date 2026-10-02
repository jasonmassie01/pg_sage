package causal

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Replication lag family: where the lag of the most lagging replica sits
// (WAL not yet sent, sent but not flushed by the standby, flushed but
// not replayed), a primary write surge that only amplifies them, and on
// a standby the replay state itself (paused, or held back by standby
// queries). Standby-side evidence is only visible from the standby.

type stage struct {
	pid                  int64
	app                  string
	send, flush, replay  float64
	writeLag, replayLagS float64
}

func stageRow(s stage) probes.Row {
	return probes.Row{"pid": s.pid, "application_name": s.app, "client_addr": "10.0.0.7",
		"state": "streaming", "sync_state": "async", "kind": "physical",
		"write_lag_s": s.writeLag, "flush_lag_s": s.writeLag, "replay_lag_s": s.replayLagS,
		"send_backlog_bytes": s.send, "flush_backlog_bytes": s.flush,
		"replay_backlog_bytes": s.replay, "replay_lag_bytes": s.send + s.flush + s.replay}
}

func replObs(ev string, at time.Duration, ss ...stage) Observation {
	var rows []probes.Row
	for _, s := range ss {
		rows = append(rows, stageRow(s))
	}
	return obsAt(ev, probes.ReplicationLag, at, rows...)
}

func primaryState(ev string, at time.Duration) Observation {
	return obsAt(ev, probes.StandbyReplayState, at, probes.Row{"in_recovery": false,
		"replay_paused": nil, "receive_replay_bytes": nil, "last_replay_age_s": nil,
		"conflicts": int64(0), "receiver_status": nil,
		"max_standby_streaming_delay_ms": int64(30000), "longest_query_s": nil,
		"server_started_at": tempReset})
}

type standby struct {
	paused    bool
	backlog   float64
	conflicts int64
	longest   float64
	delayMS   int64
}

func standbyState(ev string, at time.Duration, s standby) Observation {
	return obsAt(ev, probes.StandbyReplayState, at, probes.Row{"in_recovery": true,
		"replay_paused": s.paused, "receive_replay_bytes": s.backlog,
		"last_replay_age_s": 120.0, "conflicts": s.conflicts, "receiver_status": "streaming",
		"max_standby_streaming_delay_ms": s.delayMS, "longest_query_s": s.longest,
		"server_started_at": tempReset})
}

func walVolume(ev string, at time.Duration, bytes float64) Observation {
	return obsAt(ev, probes.WALCheckpoint, at, probes.Row{"wal_bytes": bytes,
		"wal_stats_reset": nil})
}

// primaryLag is a primary-side window: two lag samples 3 s apart with
// the WAL written in between.
func primaryLag(first, last stage, walDelta float64) []Observation {
	return []Observation{replObs("R1", 0, first), primaryState("P1", 0),
		walVolume("W1", 0, 1<<30), replObs("R2", 3*time.Second, last),
		primaryState("P2", 3*time.Second), walVolume("W2", 3*time.Second, 1<<30+walDelta)}
}

func TestReplication_ReplayBacklogUnderASurge(t *testing.T) {
	d := DiagnoseReplicationLag(primaryLag(
		stage{pid: 500, app: "replica1", replay: 20 * mib},
		stage{pid: 500, app: "replica1", send: mib, replay: 48 * mib, replayLagS: 14},
		48*mib))
	if d.Family != FamilyReplicationLag {
		t.Fatalf("family = %s", d.Family)
	}
	wantRoot(t, d, StandbyReplayBacklog)
	if d.Root.Subject != `replica "replica1" (pid 500)` {
		t.Fatalf("subject = %q", d.Root.Subject)
	}
	wantFact(t, d.Root.Support, "R2", "between flushed and replayed")
	wantFact(t, d.Root.Support, "R2", "grew")
	wantStatus(t, d, ReplicationWriteSurge, StatusContributing)
	wantStatus(t, d, WALSendBacklog, StatusRuledOut)
	wantStatus(t, d, StandbyFlushBacklog, StatusRuledOut)
	// Standby-side mechanisms are not visible from the primary.
	wantStatus(t, d, ReplayPaused, StatusAlternative)
	wantStatus(t, d, StandbyQueryDelay, StatusAlternative)
	wantMissing(t, d, probes.StandbyReplayState, "not_a_standby")
	everyHypothesisCarriesRefutation(t, d)
}

func TestReplication_SendBacklog(t *testing.T) {
	d := DiagnoseReplicationLag(primaryLag(stage{pid: 501, app: "r"},
		stage{pid: 501, app: "r", send: 40 * mib, flush: 4 * mib}, mib))
	wantRoot(t, d, WALSendBacklog)
	wantFact(t, d.Root.Support, "R2", "not yet sent")
	wantStatus(t, d, StandbyFlushBacklog, StatusRuledOut)
	wantStatus(t, d, StandbyReplayBacklog, StatusRuledOut)
	wantStatus(t, d, ReplicationWriteSurge, StatusRuledOut)
}

func TestReplication_FlushBacklog(t *testing.T) {
	d := DiagnoseReplicationLag(primaryLag(stage{pid: 502, app: "r"},
		stage{pid: 502, app: "r", flush: 48 * mib, writeLag: 11}, 48*mib))
	wantRoot(t, d, StandbyFlushBacklog)
	wantFact(t, d.Root.Support, "R2", "between sent and flushed")
	wantStatus(t, d, StandbyReplayBacklog, StatusRuledOut)
}

// Decoy: a replica that keeps up with a surge.
func TestReplication_KeepingUpIsInconclusive(t *testing.T) {
	d := DiagnoseReplicationLag(primaryLag(stage{pid: 503, app: "r"},
		stage{pid: 503, app: "r", replay: 512 << 10}, 64*mib))
	wantInconclusive(t, d)
	for _, id := range []NodeID{WALSendBacklog, StandbyFlushBacklog, StandbyReplayBacklog,
		ReplicationWriteSurge} {
		wantStatus(t, d, id, StatusRuledOut)
	}
	h, _ := hypothesisOf(d, ReplicationWriteSurge)
	wantFact(t, h.Contradict, "R2", "keeps up")
}

func TestReplication_NoReplicaConnected(t *testing.T) {
	d := DiagnoseReplicationLag([]Observation{replObs("R1", 0), primaryState("P1", 0),
		walVolume("W1", 0, 0), replObs("R2", 3*time.Second), primaryState("P2", 3*time.Second),
		walVolume("W2", 3*time.Second, 0)})
	wantInconclusive(t, d)
	h, _ := hypothesisOf(d, StandbyReplayBacklog)
	wantFact(t, h.Contradict, "R2", "no standby or replication client is connected")
}

// Boundary: 16 MiB of total lag is lag; a byte less is keeping up.
func TestReplication_LagFloorBoundary(t *testing.T) {
	at := DiagnoseReplicationLag(primaryLag(stage{pid: 504, app: "r"},
		stage{pid: 504, app: "r", replay: 16 * mib}, 0))
	wantRoot(t, at, StandbyReplayBacklog)
	below := DiagnoseReplicationLag(primaryLag(stage{pid: 504, app: "r"},
		stage{pid: 504, app: "r", replay: 16*mib - 1}, 0))
	wantStatus(t, below, StandbyReplayBacklog, StatusRuledOut)
}

// The most lagging replica is diagnosed, not the first listed.
func TestReplication_WorstReplicaIsTheSubject(t *testing.T) {
	ok := stage{pid: 505, app: "fast", replay: mib}
	slow := stage{pid: 506, app: "slow", flush: 30 * mib}
	d := DiagnoseReplicationLag(primaryLag(ok, slow, 0))
	d2 := DiagnoseReplicationLag([]Observation{replObs("R1", 0, ok, slow),
		replObs("R2", 3*time.Second, ok, slow)})
	for _, x := range []Diagnosis{d, d2} {
		wantRoot(t, x, StandbyFlushBacklog)
		if x.Root.Subject != `replica "slow" (pid 506)` {
			t.Fatalf("subject = %q", x.Root.Subject)
		}
	}
}

func TestReplication_StandbyReplayPaused(t *testing.T) {
	s := standby{paused: true, backlog: 64 * mib, delayMS: 30000}
	d := DiagnoseReplicationLag([]Observation{replObs("R1", 0), standbyState("S1", 0, s),
		replObs("R2", 3*time.Second), standbyState("S2", 3*time.Second, s)})
	wantRoot(t, d, ReplayPaused)
	wantStatus(t, d, StandbyReplayBacklog, StatusContributing)
	wantStatus(t, d, StandbyQueryDelay, StatusRuledOut)
	// The upstream's send and flush stages are not visible on a standby.
	for _, id := range []NodeID{WALSendBacklog, StandbyFlushBacklog} {
		wantStatus(t, d, id, StatusAlternative)
	}
	wantMissing(t, d, probes.ReplicationLag, "primary_side_unavailable")
}

func TestReplication_StandbyQueriesHoldBackReplay(t *testing.T) {
	first := standby{backlog: 40 * mib, conflicts: 2, longest: 600, delayMS: -1}
	last := first
	last.backlog, last.conflicts = 80*mib, 5
	d := DiagnoseReplicationLag([]Observation{standbyState("S1", 0, first),
		standbyState("S2", 3*time.Second, last)})
	wantRoot(t, d, StandbyQueryDelay)
	wantStatus(t, d, StandbyReplayBacklog, StatusContributing)
	wantStatus(t, d, ReplayPaused, StatusRuledOut)
	wantFact(t, d.Root.Support, "S2", "3 recovery conflicts")
}

func TestReplication_UnavailableEvidence(t *testing.T) {
	denied := failedObs("R1", probes.ReplicationLag, probes.StatusNoPrivilege,
		"insufficient_privilege")
	d := DiagnoseReplicationLag([]Observation{denied})
	wantInconclusive(t, d)
	wantMissing(t, d, probes.ReplicationLag, "insufficient_privilege")
	wantMissing(t, d, probes.StandbyReplayState, "not_collected")
	none := DiagnoseReplicationLag(nil)
	wantInconclusive(t, none)
	wantMissing(t, none, probes.ReplicationLag, "not_collected")
}
