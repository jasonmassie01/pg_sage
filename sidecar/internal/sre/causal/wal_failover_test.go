package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-07, WAL retention: across a failover the WAL counter, the slots
// and the archiver belong to another server (or timeline). Neither a WAL
// rate nor slot growth nor a rising failure count may be computed from
// them; the comparison is stated as missing evidence.

func withIdent(o Observation, si probes.ServerIdentity) Observation {
	for _, r := range o.Result.Rows {
		r["server_started_at"] = si.StartedAt
		r["system_identifier"] = si.SystemID
		r["timeline_id"] = si.TimelineID
		r["in_recovery"] = si.Role == probes.ServerRoleStandby
		r["server_version_num"] = si.VersionNum
	}
	return o
}

// walCaseIdent is walCase with the first samples taken on a and the
// second on b.
func walCaseIdent(a, b probes.ServerIdentity, s1, s2 []slot, walDelta float64,
	a1, a2 archiver) []Observation {
	t1 := t0.Add(5 * time.Second)
	return []Observation{
		withIdent(slotObs("E1", t0, s1...), a), withIdent(walObs("E2", t0, 1e12, walReset), a),
		archObs("E3", t0, a1),
		withIdent(slotObs("E4", t1, s2...), b),
		withIdent(walObs("E5", t1, 1e12+walDelta, walReset), b),
		archObs("E6", t1, a2),
	}
}

var promotedIdent = ident(serverStart.Add(2*time.Hour), "7001", 2, probes.ServerRolePrimary,
	170010)

func TestWAL_FailoverBetweenSamplesHasNoRate(t *testing.T) {
	// 400 MiB in 5 s would be a strong surge on one server.
	d := DiagnoseWAL(walCaseIdent(baseIdent, promotedIdent,
		[]slot{{"replica_b", true, 10 * mib}}, []slot{{"replica_b", true, 10 * mib}},
		400*mib, archiveOK, archiveOK))
	surge, st := byNode(d, WriteSurge)
	if st == StatusRoot || surge.Confidence != 0 || len(surge.Support) != 0 {
		t.Fatalf("surge scored across a failover: %s %+v", st, surge)
	}
	if got := missingReason(d, probes.WALCheckpoint); got != ReasonFailover {
		t.Fatalf("wal_checkpoint missing reason = %q, want %q (%+v)", got,
			ReasonFailover, d.Missing)
	}
	for _, f := range d.Observed {
		if strings.Contains(f.Text, "bytes/s") {
			t.Fatalf("a WAL rate was stated across a failover: %s", f.Text)
		}
	}
}

func TestWAL_FailoverBetweenSamplesHasNoSlotGrowth(t *testing.T) {
	// The new primary's slot of the same name retains much more WAL: that
	// is another slot's position, not growth.
	d := DiagnoseWAL(walCaseIdent(baseIdent, promotedIdent,
		[]slot{{"cdc", true, 2 * mib}}, []slot{{"cdc", true, 300 * mib}},
		1*mib, archiveOK, archiveOK))
	slow, st := byNode(d, SlowConsumer)
	if st == StatusRoot || slow.Confidence != 0 {
		t.Fatalf("slow consumer scored from slot growth across a failover: %s %+v",
			st, slow)
	}
	if got := missingReason(d, probes.ReplicationSlots); got != ReasonFailover {
		t.Fatalf("replication_slots missing reason = %q, want %q (%+v)", got,
			ReasonFailover, d.Missing)
	}
	inactive := DiagnoseWAL(walCaseIdent(baseIdent, promotedIdent,
		[]slot{{"old", false, 20 * mib}}, []slot{{"old", false, 400 * mib}},
		1*mib, archiveOK, archiveOK))
	h, _ := byNode(inactive, InactiveSlot)
	for _, f := range h.Support {
		if strings.Contains(f.Text, "grew") {
			t.Fatalf("inactive slot growth stated across a failover: %s", f.Text)
		}
	}
	// What the last sample shows is still an observation: an inactive slot
	// on the new primary retaining 400 MiB.
	if h.Confidence < 0.6 {
		t.Fatalf("the last sample's inactive slot was not judged: %+v", h)
	}
}

func TestWAL_FailoverBetweenSamplesHasNoRisingFailures(t *testing.T) {
	failing := archiver{mode: "on", failed: 9, lastOK: t0.Add(-time.Hour),
		lastFailedAt: t0.Add(-time.Minute)}
	before := archiver{mode: "on", failed: 2, lastOK: t0.Add(-time.Hour),
		lastFailedAt: t0.Add(-2 * time.Hour)}
	d := DiagnoseWAL(walCaseIdent(baseIdent, promotedIdent,
		nil, nil, 1*mib, before, failing))
	h, _ := byNode(d, ArchiverFailure)
	for _, f := range h.Support {
		if strings.Contains(f.Text, "rose") {
			t.Fatalf("failed_count compared across a failover: %s", f.Text)
		}
	}
}

func TestWAL_RestartBetweenSamplesHasNoRate(t *testing.T) {
	restarted := ident(serverStart.Add(time.Hour), "7001", 1, probes.ServerRolePrimary,
		170010)
	d := DiagnoseWAL(walCaseIdent(baseIdent, restarted, nil, nil, 400*mib,
		archiveOK, archiveOK))
	if got := missingReason(d, probes.WALCheckpoint); got != ReasonServerRestarted {
		t.Fatalf("wal_checkpoint missing reason = %q, want %q", got, ReasonServerRestarted)
	}
	if h, _ := byNode(d, WriteSurge); h.Confidence != 0 {
		t.Fatalf("surge scored across a restart: %+v", h)
	}
}

// Same incarnation with the identity: the M2 behavior is unchanged.
func TestWAL_SameIncarnationWithIdentityStillMeasures(t *testing.T) {
	d := DiagnoseWAL(walCaseIdent(baseIdent, baseIdent,
		[]slot{{"cdc", false, 200 * mib}}, []slot{{"cdc", false, 400 * mib}},
		200*mib, archiveOK, archiveOK))
	requireStatus(t, d, InactiveSlot, StatusRoot)
	requireStatus(t, d, WriteSurge, StatusContributing)
	if got := missingReason(d, probes.WALCheckpoint); got != "" {
		t.Fatalf("comparable WAL samples reported missing %q", got)
	}
}
