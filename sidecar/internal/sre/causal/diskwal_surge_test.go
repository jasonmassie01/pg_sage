package causal

import (
	"math"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// In the disk/WAL runway a write surge is the WAL that writes produce
// between the two close samples. WAL is recycled at checkpoints unless a
// slot or the archiver retains it (their own hypotheses), so a surge is
// not what keeps filling the disk when the database files themselves
// grow steadily: then database growth is the root and the surge only
// contributes. Without supported file growth a surge stays a root.

// surgeDelta is 200 MiB of WAL in the 5 s between the samples: 40 MiB/s,
// far above the 4 MiB/s floor and 10 times the fixture's long-run rate.
const surgeDelta = 200 * mib

func growingDatabases(rate, r2 float64) probes.Row {
	return trendRow(probes.RunwayDatabaseBytes, probes.SubjectCluster, 12, 1e9,
		math.NaN(), rate, r2)
}

// Heavy concurrent WAL inflates pg_wal, so the databases are under half
// of the disk usage growth and lose the share bonus (0.5 against the
// surge's 0.7); the files still grew steadily and materially.
func TestDiskWAL_GrowthUnderHeavyWALIsTheRoot(t *testing.T) {
	d := DiagnoseDiskWAL(diskCase(nil, nil, surgeDelta,
		growingDatabases(50000, 0.98),
		trendRow(probes.RunwayDiskUsed, probes.SubjectCluster, 12, 1.2e9, 1e11, 400000,
			0.9)))
	root := requireStatus(t, d, DatabaseGrowth, StatusRoot)
	if !d.Conclusive || !hasFact(root.Support, "E1", "50000") {
		t.Fatalf("growth root = %+v (conclusive %v)", root, d.Conclusive)
	}
	surge := requireStatus(t, d, WriteSurge, StatusContributing)
	if !(surge.Confidence >= 0.7) || !hasFact(surge.Support, "E7", "41943040") {
		t.Fatalf("contributing surge = %+v, want its measured rate cited", surge)
	}
	if !(surge.Confidence > root.Confidence) {
		t.Fatalf("fixture must have the surge outscore growth: %.2f vs %.2f",
			surge.Confidence, root.Confidence)
	}
}

// The share bonus case is unchanged: growth wins on confidence and the
// surge contributes.
func TestDiskWAL_GrowthWithShareAndSurge(t *testing.T) {
	d := DiagnoseDiskWAL(diskCase(nil, nil, surgeDelta,
		growingDatabases(50000, 0.98),
		trendRow(probes.RunwayDiskUsed, probes.SubjectCluster, 12, 1.2e9, 1e11, 52000,
			0.98)))
	requireStatus(t, d, DatabaseGrowth, StatusRoot)
	requireStatus(t, d, WriteSurge, StatusContributing)
}

// A pure WAL surge: the database files did not grow.
func TestDiskWAL_PureSurgeWithoutFileGrowth(t *testing.T) {
	d := DiagnoseDiskWAL(diskCase(nil, nil, surgeDelta, flatDatabases()))
	root := requireStatus(t, d, WriteSurge, StatusRoot)
	if !hasFact(root.Support, "E7", "41943040") {
		t.Fatalf("surge root = %+v", root)
	}
	requireStatus(t, d, DatabaseGrowth, StatusRuledOut)
	if len(d.Contributing) != 0 {
		t.Fatalf("contributing = %+v, want none", d.Contributing)
	}
}

// Growth that is not supported (immaterial, or an unsteady churn) does
// not demote a surge: the surge stays the root, growth an alternative.
func TestDiskWAL_SurgeOverUnsupportedGrowth(t *testing.T) {
	for name, growth := range map[string]probes.Row{
		"immaterial": growingDatabases(100, 0.99),
		"churn":      growingDatabases(50000, 0.3),
	} {
		t.Run(name, func(t *testing.T) {
			d := DiagnoseDiskWAL(diskCase(nil, nil, surgeDelta, growth))
			requireStatus(t, d, WriteSurge, StatusRoot)
			requireStatus(t, d, DatabaseGrowth, StatusAlternative)
		})
	}
}

// Decoys stay inconclusive: churn and a keeping-up consumer, with no
// surge between the samples.
func TestDiskWAL_DecoysWithoutSurgeStayInconclusive(t *testing.T) {
	sub := []slot{{"sub", true, mib / 2}}
	for name, o := range map[string][]Observation{
		"churn": diskCase(nil, nil, 0, growingDatabases(50000, 0.3)),
		"keeping up": diskCase(sub, sub, 0,
			trendRow(probes.RunwayWALSlot, "sub", 10, mib/2, 10<<30, 0, 1),
			growingDatabases(5, 0.3)),
	} {
		t.Run(name, func(t *testing.T) {
			if d := DiagnoseDiskWAL(o); d.Root != nil || len(d.Contributing) != 0 {
				t.Fatalf("decoy diagnosed root %+v contributing %+v", d.Root,
					d.Contributing)
			}
		})
	}
}

// A slot root keeps the WAL family's rule: the surge contributes to the
// slot, and supported growth below the slot's confidence is an
// alternative, not a second root.
func TestDiskWAL_SlotRootWithGrowthAndSurge(t *testing.T) {
	cdc := []slot{{"cdc", false, 8 * mib}}
	d := DiagnoseDiskWAL(diskCase(cdc, cdc, surgeDelta,
		trendRow(probes.RunwayWALSlot, "cdc", 10, 8*mib, 10<<30, 2000, 0.99),
		growingDatabases(50000, 0.98)))
	requireStatus(t, d, InactiveSlot, StatusRoot)
	requireStatus(t, d, WriteSurge, StatusContributing)
	requireStatus(t, d, DatabaseGrowth, StatusAlternative)
}
