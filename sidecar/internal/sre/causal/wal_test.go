package causal

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// WAL retention family (AI-SRE-SPEC §6): an inactive slot, a slow
// consumer, archiver failure and a write surge are told apart from two
// samples of the slots, WAL volume and the archiver. Slot retention and
// a write surge stay separate (CHECK-05); disk capacity is never
// inferred and stays unknown.

const mib = 1 << 20

var walReset = t0.Add(-100 * 24 * time.Hour)

type slot struct {
	name     string
	active   bool
	retained float64
}

func slotObs(id string, at time.Time, ss ...slot) Observation {
	var rows []probes.Row
	for _, s := range ss {
		rows = append(rows, probes.Row{"slot_name": s.name, "slot_type": "physical",
			"active": s.active, "wal_status": "extended", "retained_bytes": s.retained})
	}
	o := obs(id, probes.ReplicationSlots, rows...)
	o.Result.ObservedAt = at
	return o
}

func walObs(id string, at time.Time, bytes float64, reset time.Time) Observation {
	o := obs(id, probes.WALCheckpoint, probes.Row{"wal_bytes": bytes,
		"wal_stats_reset": reset})
	o.Result.ObservedAt = at
	return o
}

type archiver struct {
	mode                 string
	failed               int64
	lastOK, lastFailedAt time.Time
}

func archObs(id string, at time.Time, a archiver) Observation {
	row := probes.Row{"archive_mode": a.mode, "archived_count": int64(100),
		"failed_count": a.failed, "stats_reset": walReset}
	if !a.lastOK.IsZero() {
		row["last_archived_time"] = a.lastOK
	}
	if !a.lastFailedAt.IsZero() {
		row["last_failed_time"] = a.lastFailedAt
	}
	o := obs(id, probes.Archiver, row)
	o.Result.ObservedAt = at
	return o
}

var archiveOK = archiver{mode: "on", lastOK: t0.Add(-time.Minute)}

// walCase builds two samples 5 s apart. The lifetime average WAL rate
// is 1e12 bytes over 100 days (about 0.12 MiB/s).
func walCase(s1, s2 []slot, walDelta float64, a1, a2 archiver) []Observation {
	t1 := t0.Add(5 * time.Second)
	return []Observation{
		slotObs("E1", t0, s1...), walObs("E2", t0, 1e12, walReset),
		archObs("E3", t0, a1),
		slotObs("E4", t1, s2...), walObs("E5", t1, 1e12+walDelta, walReset),
		archObs("E6", t1, a2),
	}
}

func TestWAL_InactiveSlotWithSurgeContributing(t *testing.T) {
	d := DiagnoseWAL(walCase([]slot{{"cdc", false, 200 * mib}},
		[]slot{{"cdc", false, 260 * mib}}, 60*mib, archiveOK, archiveOK))
	root := requireStatus(t, d, InactiveSlot, StatusRoot)
	if root.Subject != `slot "cdc"` || root.Support[0].EvidenceID != "E4" {
		t.Fatalf("inactive slot root = %+v", root)
	}
	requireStatus(t, d, WriteSurge, StatusContributing)
	requireStatus(t, d, SlowConsumer, StatusRuledOut)
	requireStatus(t, d, ArchiverFailure, StatusRuledOut)
	if !hasMissing(d, "disk_capacity", "provider_metric_unavailable") {
		t.Fatalf("missing = %+v, want unknown disk capacity", d.Missing)
	}
}

func TestWAL_WriteSurgeWithoutSlots(t *testing.T) {
	d := DiagnoseWAL(walCase(nil, nil, 100*mib, archiveOK, archiveOK))
	root := requireStatus(t, d, WriteSurge, StatusRoot)
	if !strings.Contains(root.Support[0].Text, "bytes/s") {
		t.Fatalf("surge support %q must state the measured rate", root.Support[0].Text)
	}
	requireStatus(t, d, InactiveSlot, StatusRuledOut)
}

func TestWAL_SlowConsumer(t *testing.T) {
	d := DiagnoseWAL(walCase([]slot{{"standby", true, 100 * mib}},
		[]slot{{"standby", true, 150 * mib}}, 50*mib, archiveOK, archiveOK))
	requireStatus(t, d, SlowConsumer, StatusRoot)
	inactive := requireStatus(t, d, InactiveSlot, StatusRuledOut)
	if !strings.Contains(inactive.Contradict[0].Text, "active") {
		t.Fatalf("inactive-slot contradiction = %+v", inactive.Contradict)
	}
}

func TestWAL_ArchiverFailure(t *testing.T) {
	failing := archiver{mode: "on", failed: 3, lastOK: t0.Add(-time.Hour),
		lastFailedAt: t0.Add(-time.Second)}
	later := failing
	later.failed, later.lastFailedAt = 5, t0.Add(4*time.Second)
	d := DiagnoseWAL(walCase(nil, nil, mib, failing, later))
	root := requireStatus(t, d, ArchiverFailure, StatusRoot)
	if !strings.Contains(root.Support[0].Text+root.Support[1].Text, "3 to 5") {
		t.Fatalf("archiver support = %+v, want the failed_count rise", root.Support)
	}
}

func TestWAL_ArchivingOffRulesOutTheArchiver(t *testing.T) {
	off := archiver{mode: "off"}
	d := DiagnoseWAL(walCase(nil, nil, mib, off, off))
	h := requireStatus(t, d, ArchiverFailure, StatusRuledOut)
	if !strings.Contains(h.Contradict[0].Text, "archive_mode is off") {
		t.Fatalf("contradiction = %+v", h.Contradict)
	}
}

// CHECK-06: steady WAL, no slots and a healthy archiver is no incident.
func TestWAL_HealthyIsInconclusive(t *testing.T) {
	d := DiagnoseWAL(walCase(nil, nil, 64*1024, archiveOK, archiveOK))
	if d.Conclusive || d.Root != nil {
		t.Fatalf("healthy WAL = %+v, want inconclusive", d)
	}
	requireStatus(t, d, WriteSurge, StatusRuledOut)
}

// CHECK-07: a statistics reset between the samples makes the WAL rate
// unknown instead of a negative or inflated number.
func TestWAL_CounterResetInvalidatesTheRate(t *testing.T) {
	obs := walCase(nil, nil, 100*mib, archiveOK, archiveOK)
	obs[4] = walObs("E5", t0.Add(5*time.Second), 10*mib, t0.Add(time.Second))
	d := DiagnoseWAL(obs)
	h := requireStatus(t, d, WriteSurge, StatusAlternative)
	if len(h.Support)+len(h.Contradict) != 0 {
		t.Fatalf("surge used an invalid rate: %+v", h)
	}
	if !hasMissing(d, probes.WALCheckpoint, "counter_reset") {
		t.Fatalf("missing = %+v, want counter_reset", d.Missing)
	}
}

// One sample: an inactive slot retaining WAL is still a supported cause;
// rates are missing, not zero.
func TestWAL_SingleSample(t *testing.T) {
	d := DiagnoseWAL([]Observation{slotObs("E1", t0, slot{"cdc", false, 512 * mib}),
		walObs("E2", t0, 1e12, walReset), archObs("E3", t0, archiveOK)})
	requireStatus(t, d, InactiveSlot, StatusRoot)
	requireStatus(t, d, WriteSurge, StatusAlternative)
	if !hasMissing(d, probes.WALCheckpoint, "second_sample_unavailable") {
		t.Fatalf("missing = %+v", d.Missing)
	}
}

func TestWAL_UnavailableProbesAreNamed(t *testing.T) {
	d := DiagnoseWAL([]Observation{
		unavailable("E1", probes.ReplicationSlots, probes.StatusNoPrivilege),
		unavailable("E2", probes.Archiver, probes.StatusError)})
	if d.Conclusive || !hasMissing(d, probes.ReplicationSlots, "insufficient_privilege") ||
		!hasMissing(d, probes.WALCheckpoint, "not_collected") {
		t.Fatalf("diagnosis = %+v, want the unavailable probes named", d)
	}
}
