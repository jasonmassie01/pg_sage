package autovacuum

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

var env = candidate.Env{Database: "db1", WindowDays: 90}

// series turns day values into the section's series (-1: no snapshot).
func series(vals ...int64) []*int64 {
	out := make([]*int64, len(vals))
	for i, v := range vals {
		if v >= 0 {
			n := v
			out[i] = &n
		}
	}
	return out
}

// orders is a 10M-row table whose dead tuples grow 600k a day (median of
// the rises) and peak at 2M, 20% of its rows, with no table options.
func orders() workpack.AutovacuumFact {
	return workpack.AutovacuumFact{ID: "table/av/public.orders", Table: "public.orders",
		Autovacuums: int64(3), Autoanalyzes: int64(5), LastAutovacuum: "absent",
		LastAutoanalyze: "absent", LiveTuples: int64(10_000_000),
		DeadTuples: series(100_000, 600_000, 1_200_000, 300_000, 900_000, 1_500_000,
			2_000_000),
		Reloptions: map[string]string{}, ReloptionsText: []string{}}
}

func globalSetting(name, value, unit string) workpack.SettingFact {
	return workpack.SettingFact{ID: "setting/" + name, Name: name, Value: value, Unit: unit,
		Source: "default", Changes: []workpack.SettingChange{}}
}

// stockSettings are PostgreSQL's autovacuum defaults.
func stockSettings() *workpack.Settings {
	u := "unknown"
	return &workpack.Settings{Status: "ok", Settings: []workpack.SettingFact{
		globalSetting("autovacuum_analyze_scale_factor", "0.1", ""),
		globalSetting("autovacuum_naptime", "60", "s"),
		globalSetting("autovacuum_vacuum_scale_factor", "0.2", ""),
		globalSetting("autovacuum_vacuum_threshold", "50", ""),
	}, Measures: workpack.Measures{TempBytesPerHour: u, CheckpointsRequested: u,
		CheckpointsTimed: u, WALBytesPerHour: u, PeakClientBackends: u, UsableConnections: u},
		Hardware: workpack.Hardware{VCPUs: u, MemoryBytes: u}}
}

// packOf seals tables (nil: no autovacuum section) with settings.
func packOf(t *testing.T, settings *workpack.Settings,
	tables ...workpack.AutovacuumFact) *workpack.Pack {
	t.Helper()
	spec := reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90),
		Settings: settings, Platform: "postgres", PGVersion: "17.2"}
	if tables != nil {
		days := make([]string, len(tables[0].DeadTuples))
		for i := range days {
			days[i] = "2026-10-" + string(rune('1'+i/10)) + string(rune('0'+i%10))
		}
		spec.Autovacuum = &workpack.Autovacuum{Tables: tables, Total: len(tables), Days: days}
	}
	p, err := reviewtest.Build(spec)
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

func generate(t *testing.T, p *workpack.Pack) ([]candidate.Candidate, candidate.Coverage) {
	t.Helper()
	cands, cov, err := New().Generate(context.Background(), p, env)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return cands, cov
}
