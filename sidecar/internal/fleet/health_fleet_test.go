package fleet

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// healthManager builds a manager whose collector runs every 60 s.
func healthManager() *DatabaseManager {
	return NewManager(&config.Config{
		Mode:      "fleet",
		Collector: config.CollectorConfig{IntervalSeconds: 60},
	})
}

func registerHealthDB(
	mgr *DatabaseManager, name string, status InstanceStatus,
) *DatabaseInstance {
	s := status
	inst := &DatabaseInstance{
		Name:   name,
		Config: config.DatabaseConfig{Name: name},
		Status: &s,
	}
	mgr.RegisterInstance(inst)
	return inst
}

func freshStatus() InstanceStatus {
	return InstanceStatus{
		Connected: true, LastSeen: time.Now(),
		CollectorLastRun: time.Now(),
	}
}

func rowByName(t *testing.T, o FleetOverview, name string) DatabaseStatus {
	t.Helper()
	for _, db := range o.Databases {
		if db.Name == name {
			return db
		}
	}
	t.Fatalf("database %q missing from overview", name)
	return DatabaseStatus{}
}

// Equal scores must come back in name order on every refresh, or the
// Overview tiles and the per-instance gauge reorder at random.
func TestFleetStatus_EqualScoresSortByName(t *testing.T) {
	mgr := healthManager()
	for _, name := range []string{"echo", "charlie", "alpha", "delta", "bravo"} {
		registerHealthDB(mgr, name, freshStatus())
	}
	want := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for call := 0; call < 100; call++ {
		got := mgr.FleetStatus().Databases
		if len(got) != len(want) {
			t.Fatalf("call %d: %d databases, want %d", call, len(got), len(want))
		}
		for i, db := range got {
			if db.Name != want[i] {
				t.Fatalf("call %d: position %d = %q, want %q (all scores %d)",
					call, i, db.Name, want[i], db.Status.HealthScore)
			}
		}
	}
}

// Worst first still holds; the name only breaks ties.
func TestFleetStatus_WorstFirstThenName(t *testing.T) {
	mgr := healthManager()
	registerHealthDB(mgr, "zeta", freshStatus())
	crit := freshStatus()
	crit.FindingsCritical = 1
	registerHealthDB(mgr, "yankee", crit)
	registerHealthDB(mgr, "alpha", freshStatus())
	down := freshStatus()
	down.Connected = false
	registerHealthDB(mgr, "mike", down)

	got := mgr.FleetStatus().Databases
	want := []string{"mike", "yankee", "alpha", "zeta"}
	for i, db := range got {
		if db.Name != want[i] {
			t.Fatalf("position %d = %q (score %d), want %q",
				i, db.Name, db.Status.HealthScore, want[i])
		}
	}
}

// Four criticals score 0; a connected database at 0 is not healthy.
func TestFleetStatus_FourCriticalsConnectedNotHealthy(t *testing.T) {
	mgr := healthManager()
	s := freshStatus()
	s.FindingsCritical = 4
	registerHealthDB(mgr, "bad", s)
	o := mgr.FleetStatus()
	row := rowByName(t, o, "bad")
	if row.Status.HealthScore != 0 || !row.Status.Connected {
		t.Fatalf("score %d connected %v, want 0 and connected",
			row.Status.HealthScore, row.Status.Connected)
	}
	if o.Summary.Healthy != 0 || o.Summary.Degraded != 1 {
		t.Fatalf("healthy %d degraded %d, want 0 and 1",
			o.Summary.Healthy, o.Summary.Degraded)
	}
}

// Healthy means connected, no error and a score of at least 50.
func TestFleetStatus_HealthyFloorAtFifty(t *testing.T) {
	mgr := healthManager()
	atFloor := freshStatus()
	atFloor.FindingsCritical = 2 // 100 - 50 = 50
	registerHealthDB(mgr, "at-floor", atFloor)
	below := freshStatus()
	below.FindingsCritical = 2
	below.FindingsWarning = 1 // 45
	registerHealthDB(mgr, "below", below)
	errored := freshStatus()
	errored.Error = "permission denied"
	registerHealthDB(mgr, "errored", errored)

	o := mgr.FleetStatus()
	if got := rowByName(t, o, "at-floor").Status.HealthScore; got != 50 {
		t.Fatalf("at-floor score %d, want 50", got)
	}
	if got := rowByName(t, o, "below").Status.HealthScore; got != 45 {
		t.Fatalf("below score %d, want 45", got)
	}
	if o.Summary.Healthy != 1 || o.Summary.Degraded != 2 {
		t.Fatalf("healthy %d degraded %d, want 1 and 2",
			o.Summary.Healthy, o.Summary.Degraded)
	}
}

// A collector silent for more than 3 intervals caps the score at 25.
func TestFleetStatus_StaleCollectorBoundary(t *testing.T) {
	mgr := healthManager()
	fresh := freshStatus()
	fresh.CollectorLastRun = time.Now().Add(-174 * time.Second) // 2.9x
	registerHealthDB(mgr, "fresh", fresh)
	stale := freshStatus()
	stale.CollectorLastRun = time.Now().Add(-186 * time.Second) // 3.1x
	registerHealthDB(mgr, "stale", stale)

	o := mgr.FleetStatus()
	if got := rowByName(t, o, "fresh").Status.HealthScore; got != 100 {
		t.Fatalf("2.9x score %d, want 100", got)
	}
	if got := rowByName(t, o, "stale").Status.HealthScore; got != 25 {
		t.Fatalf("3.1x score %d, want 25", got)
	}
	if o.Summary.Healthy != 1 {
		t.Fatalf("healthy %d, want 1: a stale database is not healthy",
			o.Summary.Healthy)
	}
}

// The per-database interval wins over the global collector interval.
func TestFleetStatus_StalenessUsesPerDatabaseInterval(t *testing.T) {
	mgr := healthManager()
	s := freshStatus()
	s.CollectorLastRun = time.Now().Add(-20 * time.Minute)
	inst := &DatabaseInstance{
		Name: "slow",
		Config: config.DatabaseConfig{
			Name: "slow", CollectorIntervalSeconds: 600,
		},
		Status: &s,
	}
	mgr.RegisterInstance(inst)
	registerHealthDB(mgr, "global", s) // 20 min against 60 s is stale

	o := mgr.FleetStatus()
	if got := rowByName(t, o, "slow").Status.HealthScore; got != 100 {
		t.Fatalf("slow (600 s interval, 2x) score %d, want 100", got)
	}
	if got := rowByName(t, o, "global").Status.HealthScore; got != 25 {
		t.Fatalf("global (60 s interval, 20x) score %d, want 25", got)
	}
}
