package causal

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Disk / WAL runway (AI-SRE-SPEC §4 R2): what fills the disk or a slot's
// configured WAL maximum over hours, from the runway monitor's sampled
// trends plus two close samples of the slots, WAL volume and archiver.
// A slow fill is invisible in a 5 s window, so the trend decides; disk
// capacity is only ever the operator's declared capacity.

func walDirObs(id string, ready int64) Observation {
	return obs(id, probes.WALDirectoryProbe, probes.Row{"wal_dir_bytes": int64(64 << 20),
		"wal_files": int64(4), "archive_ready_files": ready})
}

// diskCase: the trends, then two samples 5 s apart of slots, WAL volume
// and the archiver (healthy), with walDelta bytes written between them.
func diskCase(s1, s2 []slot, walDelta float64, trends ...probes.Row) []Observation {
	t1 := t0.Add(5 * time.Second)
	return []Observation{
		trendsObs("E1", trends...),
		slotObs("E2", t0, s1...), walObs("E3", t0, 1e12, walReset),
		archObs("E4", t0, archiveOK), walDirObs("E5", 0),
		slotObs("E6", t1, s2...), walObs("E7", t1, 1e12+walDelta, walReset),
		archObs("E8", t1, archiveOK),
	}
}

func flatDatabases() probes.Row {
	return trendRow(probes.RunwayDatabaseBytes, probes.SubjectCluster, 12, 1e9,
		math.NaN(), 0, 1)
}

func TestDiskWAL_InactiveSlotSlowFill(t *testing.T) {
	cdc := []slot{{"cdc", false, 8 * mib}}
	d := DiagnoseDiskWAL(diskCase(cdc, cdc, 0,
		trendRow(probes.RunwayWALSlot, "cdc", 10, 8*mib, 10<<30, 2000, 0.99),
		flatDatabases()))
	if d.Family != FamilyDiskWAL {
		t.Fatalf("family = %s", d.Family)
	}
	root := requireStatus(t, d, InactiveSlot, StatusRoot)
	if root.Subject != `slot "cdc"` || !hasFact(root.Support, "E1", "2000") {
		t.Fatalf("inactive root = %+v, want the trend growth cited", root)
	}
	requireStatus(t, d, SlowConsumer, StatusRuledOut)
	requireStatus(t, d, ArchiverFailure, StatusRuledOut)
	requireStatus(t, d, WriteSurge, StatusRuledOut)
	growth := requireStatus(t, d, DatabaseGrowth, StatusRuledOut)
	if !strings.Contains(growth.Contradict[0].Text, "did not grow") {
		t.Fatalf("growth contradiction = %+v", growth.Contradict)
	}
	if !hasObserved(d, "E1", `slot "cdc"`, "10737418240", "5364514.82") {
		t.Fatalf("observed = %+v, want the slot's projected seconds", d.Observed)
	}
	if !hasMissing(d, "disk_capacity", "not_declared") {
		t.Fatalf("missing = %+v, want undeclared disk capacity", d.Missing)
	}
}

// A connected consumer that falls behind slowly shows no growth in a 5 s
// window; its trend still names it.
func TestDiskWAL_SlowConsumerByTrend(t *testing.T) {
	sub := []slot{{"sub", true, 4 * mib}}
	d := DiagnoseDiskWAL(diskCase(sub, sub, 0,
		trendRow(probes.RunwayWALSlot, "sub", 10, 4*mib, 10<<30, 3000, 0.95),
		flatDatabases()))
	root := requireStatus(t, d, SlowConsumer, StatusRoot)
	if root.Subject != `slot "sub"` || !hasFact(root.Support, "E1", "3000") {
		t.Fatalf("slow consumer root = %+v", root)
	}
	requireStatus(t, d, InactiveSlot, StatusRuledOut)
}

// Decoy: an active slot whose consumer keeps up (flat trend).
func TestDiskWAL_ConsumerKeepingUpIsInconclusive(t *testing.T) {
	sub := []slot{{"sub", true, mib / 2}}
	d := DiagnoseDiskWAL(diskCase(sub, sub, 0,
		trendRow(probes.RunwayWALSlot, "sub", 10, mib/2, 10<<30, 0, 1),
		trendRow(probes.RunwayDatabaseBytes, probes.SubjectCluster, 12, 1e9,
			math.NaN(), 5, 0.3)))
	if d.Root != nil {
		t.Fatalf("decoy produced root %+v", d.Root)
	}
	slow := requireStatus(t, d, SlowConsumer, StatusRuledOut)
	if !strings.Contains(slow.Contradict[0].Text, "keeps up") {
		t.Fatalf("contradiction = %+v", slow.Contradict)
	}
}

func TestDiskWAL_DatabaseGrowthFillsTheDeclaredDisk(t *testing.T) {
	d := DiagnoseDiskWAL(diskCase(nil, nil, 0,
		trendRow(probes.RunwayDatabaseBytes, probes.SubjectCluster, 12, 1e9,
			math.NaN(), 50000, 0.98),
		trendRow(probes.RunwayDiskUsed, probes.SubjectCluster, 12, 1.2e9, 1e11, 52000,
			0.98)))
	root := requireStatus(t, d, DatabaseGrowth, StatusRoot)
	if root.Subject != "databases" || !(root.Confidence >= 0.7) ||
		!hasFact(root.Support, "E1", "50000") {
		t.Fatalf("growth root = %+v", root)
	}
	for _, id := range []NodeID{InactiveSlot, SlowConsumer} {
		h := requireStatus(t, d, id, StatusRuledOut)
		if !strings.Contains(h.Contradict[0].Text, "no replication slots exist") {
			t.Fatalf("%s contradiction = %+v", id, h.Contradict)
		}
	}
	if !hasObserved(d, "E1", "100000000000", "1900000") {
		t.Fatalf("observed = %+v, want the disk's projected seconds", d.Observed)
	}
	if hasMissing(d, "disk_capacity", "not_declared") {
		t.Fatal("a declared capacity was reported missing")
	}
}

// Decoy: churn (load, then delete) is not steady growth; boundaries on
// the trend's fit (r2 0.6) and on the growth's size (1 MiB, 1%).
func TestDiskWAL_GrowthNeedsASteadyMaterialTrend(t *testing.T) {
	cases := []struct {
		name       string
		rate, r2   float64
		wantGrowth Status
	}{
		{"churn", 30000, 0.2, StatusAlternative},
		{"fit boundary", 50000, 0.6, StatusRoot},
		{"just under the fit", 50000, math.Nextafter(0.6, 0), StatusAlternative},
		{"too small", 100, 1, StatusAlternative},
		{"falling", -10, 0.9, StatusRuledOut},
	}
	for _, c := range cases {
		d := DiagnoseDiskWAL(diskCase(nil, nil, 0, trendRow(probes.RunwayDatabaseBytes,
			probes.SubjectCluster, 12, 1e9, math.NaN(), c.rate, c.r2)))
		if _, got := byNode(d, DatabaseGrowth); got != c.wantGrowth {
			t.Errorf("%s: database growth is %q, want %q", c.name, got, c.wantGrowth)
		}
	}
}

// Error propagation: without trends the two-sample evidence still names
// a large inactive slot; the missing trend is reported, never zero.
func TestDiskWAL_TrendsUnavailable(t *testing.T) {
	big := []slot{{"cdc", false, 200 * mib}}
	obs := diskCase(big, big, 0)
	obs[0] = unavailable("E1", probes.RunwayTrendsProbe, probes.StatusUnsupported)
	d := DiagnoseDiskWAL(obs)
	requireStatus(t, d, InactiveSlot, StatusRoot)
	requireStatus(t, d, DatabaseGrowth, StatusAlternative)
	if !hasMissingStatus(d, probes.RunwayTrendsProbe, probes.StatusUnsupported) {
		t.Fatalf("missing = %+v", d.Missing)
	}
}

// Segments waiting for the archiver add to a failing archiver; a denied
// WAL directory is missing evidence.
func TestDiskWAL_ArchiverBacklogAndDeniedDirectory(t *testing.T) {
	failing := archiver{mode: "on", failed: 3, lastOK: t0.Add(-time.Hour),
		lastFailedAt: t0.Add(-time.Second)}
	obs := diskCase(nil, nil, 0, flatDatabases())
	obs[3] = archObs("E4", t0, failing)
	obs[4] = walDirObs("E5", 12)
	obs[7] = archObs("E8", t0.Add(5*time.Second), failing)
	d := DiagnoseDiskWAL(obs)
	root := requireStatus(t, d, ArchiverFailure, StatusRoot)
	if !hasFact(root.Support, "E5", "12") {
		t.Fatalf("archiver root = %+v, want the ready segments cited", root)
	}
	obs[4] = unavailable("E5", probes.WALDirectoryProbe, probes.StatusNoPrivilege)
	d = DiagnoseDiskWAL(obs)
	if !hasMissingStatus(d, probes.WALDirectoryProbe, probes.StatusNoPrivilege) {
		t.Fatalf("missing = %+v", d.Missing)
	}
}

func hasFact(fs []Fact, ev, part string) bool {
	for _, f := range fs {
		if f.EvidenceID == ev && strings.Contains(f.Text, part) {
			return true
		}
	}
	return false
}

// Boundary: growth under 1 MiB is not material even when it is over 1%
// of a small database.
func TestDiskWAL_GrowthUnderOneMiBIsNotMaterial(t *testing.T) {
	small := 50.0 * mib
	for _, c := range []struct {
		rate float64
		want Status
	}{{float64(mib-1) / 3600, StatusAlternative}, {float64(mib) / 3600, StatusRoot}} {
		d := DiagnoseDiskWAL(diskCase(nil, nil, 0, trendRow(probes.RunwayDatabaseBytes,
			probes.SubjectCluster, 12, small, math.NaN(), c.rate, 1)))
		if _, got := byNode(d, DatabaseGrowth); got != c.want {
			t.Errorf("growth of %v bytes/s is %q, want %q", c.rate, got, c.want)
		}
	}
}

// Decoy: a consumer that keeps up still leaves a few kilobytes more each
// sample; growth under 1 MiB over the span is not a slow consumer.
func TestDiskWAL_ImmaterialSlotGrowthIsNotSlow(t *testing.T) {
	sub := []slot{{"sub", true, 64 << 10}}
	tiny := trendRow(probes.RunwayWALSlot, "sub", 4, 64<<10, 10<<30, 100, 0.99)
	tiny["first_at"] = t0.Add(-1200 * time.Millisecond)
	d := DiagnoseDiskWAL(diskCase(sub, sub, 0, tiny, flatDatabases()))
	if d.Root != nil {
		t.Fatalf("immaterial slot growth produced root %+v", d.Root)
	}
	requireStatus(t, d, SlowConsumer, StatusAlternative)
}
