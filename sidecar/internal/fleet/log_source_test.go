package fleet

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// Happy path: the snapshot carries the live log-source status.
func TestSnapshotStatusCarriesLogSource(t *testing.T) {
	at := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	inst := &DatabaseInstance{Name: "app", Status: &InstanceStatus{Connected: true},
		LogSource: func() logwatch.SourceStatus {
			return logwatch.SourceStatus{State: logwatch.SourceActive,
				File: "/pg/log/postgresql.json", Format: "jsonlog", LastLineAt: &at}
		}}
	got := inst.SnapshotStatus().LogSource
	if got.State != logwatch.SourceActive || got.File != "/pg/log/postgresql.json" ||
		got.LastLineAt == nil || !got.LastLineAt.Equal(at) {
		t.Fatalf("log source = %+v", got)
	}
}

// Nil/empty: an instance with no log source reports disabled with a reason.
func TestSnapshotStatusWithoutLogSourceIsDisabled(t *testing.T) {
	for name, inst := range map[string]*DatabaseInstance{
		"no source": {Name: "a", Status: &InstanceStatus{}},
		"no status": {Name: "b"},
		"zero status": {Name: "c",
			LogSource: func() logwatch.SourceStatus { return logwatch.SourceStatus{} }},
	} {
		got := inst.SnapshotStatus().LogSource
		if got.State != logwatch.SourceDisabled || got.Reason == "" {
			t.Fatalf("%s: log source = %+v, want disabled with a reason", name, got)
		}
	}
}

// Fleet: FleetStatus shows each database's own log source.
func TestFleetStatusLogSourcePerDatabase(t *testing.T) {
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{Name: "on",
		Config: config.DatabaseConfig{Name: "on"}, Status: &InstanceStatus{Connected: true},
		LogSource: func() logwatch.SourceStatus {
			return logwatch.UnavailableStatus("log_destination is \"stderr\"")
		}})
	mgr.RegisterInstance(&DatabaseInstance{Name: "off",
		Config: config.DatabaseConfig{Name: "off"}, Status: &InstanceStatus{Connected: true}})
	states := map[string]logwatch.SourceStatus{}
	for _, db := range mgr.FleetStatus().Databases {
		states[db.Name] = db.Status.LogSource
	}
	if st := states["on"]; st.State != logwatch.SourceUnavailable ||
		!strings.Contains(st.Reason, "log_destination") {
		t.Fatalf("on = %+v", st)
	}
	if st := states["off"]; st.State != logwatch.SourceDisabled {
		t.Fatalf("off = %+v", st)
	}
}

// Concurrency: snapshots taken while status writers run.
func TestSnapshotStatusLogSourceConcurrent(t *testing.T) {
	inst := &DatabaseInstance{Name: "app", Status: &InstanceStatus{},
		LogSource: func() logwatch.SourceStatus {
			return logwatch.SourceStatus{State: logwatch.SourceActive}
		}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				inst.UpdateStatus(func(s *InstanceStatus) { s.FindingsOpen++ })
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if inst.SnapshotStatus().LogSource.State != logwatch.SourceActive {
					t.Error("snapshot lost the log source")
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := inst.SnapshotStatus().FindingsOpen; got != 800 {
		t.Fatalf("findings = %d, want 800", got)
	}
}
