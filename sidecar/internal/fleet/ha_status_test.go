package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/ha"
)

// fleet-04: the fleet status shows each database's HA role, safe mode
// and last role change, read from its one monitor's snapshot.

func flipMonitor(roles ...bool) *ha.Monitor {
	i := 0
	return ha.NewFromProbe(func(context.Context) (bool, error) {
		r := roles[i]
		if i < len(roles)-1 {
			i++
		}
		return r, nil
	}, func(string, string, ...any) {})
}

func TestSnapshotStatusCarriesTheHASnapshot(t *testing.T) {
	m := flipMonitor(false, true, false)
	inst := &DatabaseInstance{Name: "orders", HA: m, Status: &InstanceStatus{}}
	if s := inst.SnapshotStatus(); s.HARole != "unknown" || s.HASafeMode ||
		s.HALastChange != nil {
		t.Fatalf("before a check: role %q safe %v change %v, want unknown", s.HARole,
			s.HASafeMode, s.HALastChange)
	}
	ctx := context.Background()
	m.Check(ctx)
	if s := inst.SnapshotStatus(); s.HARole != "primary" || s.HASafeMode ||
		s.HALastChange != nil {
		t.Fatalf("stable primary: role %q safe %v change %v", s.HARole, s.HASafeMode,
			s.HALastChange)
	}
	m.Check(ctx)
	m.Check(ctx)
	s := inst.SnapshotStatus()
	want := m.Snapshot().LastChange
	if s.HARole != "primary" || !s.HASafeMode || s.HALastChange == nil ||
		!s.HALastChange.Equal(want) {
		t.Fatalf("after two flips: role %q safe %v change %v, want safe mode changed at %v",
			s.HARole, s.HASafeMode, s.HALastChange, want)
	}
	if inst.Status.HARole != "" {
		t.Fatal("SnapshotStatus wrote the HA role into the stored status")
	}
}

func TestSnapshotStatusWithoutAMonitorIsUnknown(t *testing.T) {
	s := (&DatabaseInstance{Name: "orders"}).SnapshotStatus()
	if s.HARole != "unknown" || s.HASafeMode || s.HALastChange != nil {
		t.Fatalf("no monitor: role %q safe %v change %v, want unknown", s.HARole,
			s.HASafeMode, s.HALastChange)
	}
}

// GET /api/v1/databases serializes ha_role and ha_safe_mode per database;
// ha_last_change only once a role changed.
func TestFleetStatusJSONShowsTheHAFields(t *testing.T) {
	m := flipMonitor(true)
	m.Check(context.Background())
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&DatabaseInstance{Name: "orders", HA: m,
		Status: &InstanceStatus{Connected: true}})
	body, err := json.Marshal(mgr.FleetStatus())
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	for _, want := range []string{`"ha_role":"replica"`, `"ha_safe_mode":false`} {
		if !strings.Contains(got, want) {
			t.Errorf("fleet JSON lacks %s: %s", want, got)
		}
	}
	if strings.Contains(got, "ha_last_change") {
		t.Errorf("fleet JSON shows a last change before any change: %s", got)
	}
}
