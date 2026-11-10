package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// collection-08: GET /api/v1/databases carries each collector's health.

func TestSnapshotStatusCarriesCollectorHealth(t *testing.T) {
	inst := &DatabaseInstance{Name: "app", Status: &InstanceStatus{Connected: true},
		Collector: collector.New(nil, config.DefaultConfig(), 170000,
			func(string, string, ...any) {})}
	st := inst.SnapshotStatus()
	if st.Collector == nil || st.Collector.BreakerState != collector.BreakerClosed {
		t.Fatalf("Collector = %+v, want a closed breaker", st.Collector)
	}
	if inst.Status.Collector != nil {
		t.Error("SnapshotStatus wrote the collector status into the shared Status")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"collector":{`, `"breaker_state":"closed"`,
		`"unavailable":{}`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("status JSON lacks %s: %s", key, raw)
		}
	}
}

func TestSnapshotStatusWithoutCollectorOmitsHealth(t *testing.T) {
	inst := &DatabaseInstance{Name: "down", Status: &InstanceStatus{Error: "refused"}}
	st := inst.SnapshotStatus()
	if st.Collector != nil {
		t.Errorf("Collector = %+v for an instance without one, want nil", st.Collector)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), `"collector"`) {
		t.Errorf("status JSON names a collector that does not exist: %s", raw)
	}
}

func TestFleetStatusListsCollectorPerDatabase(t *testing.T) {
	mgr := NewManager(config.DefaultConfig())
	for _, name := range []string{"a", "b"} {
		mgr.RegisterInstance(&DatabaseInstance{Name: name, Status: &InstanceStatus{},
			Collector: collector.New(nil, config.DefaultConfig(), 160000,
				func(string, string, ...any) {})})
	}
	got := 0
	for _, db := range mgr.FleetStatus().Databases {
		if db.Status.Collector == nil {
			t.Errorf("database %q has no collector status", db.Name)
			continue
		}
		if db.Status.Collector.Capabilities.PGVersionNum != 160000 {
			t.Errorf("database %q capabilities = %+v", db.Name,
				db.Status.Collector.Capabilities)
		}
		got++
	}
	if got != 2 {
		t.Errorf("collector statuses = %d, want 2", got)
	}
}
