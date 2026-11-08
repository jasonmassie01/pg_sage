package fleet

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var healthNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

const healthInterval = time.Minute

func connectedAt(lastRun time.Time) *InstanceStatus {
	return &InstanceStatus{Connected: true, CollectorLastRun: lastRun}
}

func ago(d time.Duration) time.Time { return healthNow.Add(-d) }

func assertHealth(
	t *testing.T, s *InstanceStatus, registered time.Time,
	wantScore int, wantReasons []string,
) {
	t.Helper()
	score, reasons := healthOf(s, healthInterval, registered, healthNow)
	if score != wantScore {
		t.Fatalf("score %d, want %d (reasons %q)", score, wantScore, reasons)
	}
	if !reflect.DeepEqual(reasons, wantReasons) {
		t.Fatalf("reasons %q, want %q", reasons, wantReasons)
	}
}

func TestHealthOf_StalenessBoundary(t *testing.T) {
	registered := ago(24 * time.Hour)
	assertHealth(t, connectedAt(ago(174*time.Second)), registered, 100, []string{})
	assertHealth(t, connectedAt(ago(3*time.Minute)), registered, 100, []string{})
	assertHealth(t, connectedAt(ago(186*time.Second)), registered, 25,
		[]string{"collector stale 3m"})
	assertHealth(t, connectedAt(ago(42*time.Minute)), registered, 25,
		[]string{"collector stale 42m"})
}

func TestHealthOf_ZeroLastRun(t *testing.T) {
	// A new instance has not had time for its first sample.
	assertHealth(t, connectedAt(time.Time{}), ago(2*time.Minute), 100, []string{})
	assertHealth(t, connectedAt(time.Time{}), ago(3*time.Minute), 100, []string{})
	// One registered more than 3 intervals ago has gone silent.
	assertHealth(t, connectedAt(time.Time{}), ago(186*time.Second), 25,
		[]string{"collector stale: no sample in 3m"})
	// With no registration time its age is unknown: no penalty.
	assertHealth(t, connectedAt(time.Time{}), time.Time{}, 100, []string{})
}

func TestHealthOf_NoIntervalNeverStale(t *testing.T) {
	// A zero interval gives no yardstick, so staleness cannot be judged.
	score, reasons := healthOf(connectedAt(ago(48*time.Hour)), 0,
		ago(72*time.Hour), healthNow)
	if score != 100 || len(reasons) != 0 {
		t.Fatalf("score %d reasons %q, want 100 and none", score, reasons)
	}
}

func TestHealthOf_FutureLastRunIsFresh(t *testing.T) {
	// Clock skew between hosts must not read as staleness.
	assertHealth(t, connectedAt(healthNow.Add(time.Hour)), ago(time.Hour), 100,
		[]string{})
}

func TestHealthOf_FindingsReasons(t *testing.T) {
	s := connectedAt(ago(time.Second))
	s.FindingsCritical, s.FindingsWarning = 2, 5
	assertHealth(t, s, ago(time.Hour), 25,
		[]string{"2 critical findings", "5 warnings"})
	one := connectedAt(ago(time.Second))
	one.FindingsCritical, one.FindingsWarning = 1, 1
	assertHealth(t, one, ago(time.Hour), 70,
		[]string{"1 critical finding", "1 warning"})
	four := connectedAt(ago(time.Second))
	four.FindingsCritical = 4
	assertHealth(t, four, ago(time.Hour), 0, []string{"4 critical findings"})
}

func TestHealthOf_StaleCapNeverRaisesScore(t *testing.T) {
	s := connectedAt(ago(time.Hour))
	s.FindingsCritical = 4
	assertHealth(t, s, ago(2*time.Hour), 0,
		[]string{"collector stale 1h", "4 critical findings"})
	w := connectedAt(ago(time.Hour))
	w.FindingsWarning = 1
	assertHealth(t, w, ago(2*time.Hour), 25,
		[]string{"collector stale 1h", "1 warning"})
}

func TestHealthOf_DisconnectedAndError(t *testing.T) {
	down := &InstanceStatus{Error: "unreachable: connection refused",
		CollectorLastRun: ago(time.Second)}
	assertHealth(t, down, ago(time.Hour), 0,
		[]string{"disconnected: unreachable: connection refused"})
	bare := &InstanceStatus{CollectorLastRun: ago(time.Second)}
	assertHealth(t, bare, ago(time.Hour), 0, []string{"disconnected"})
	errored := connectedAt(ago(time.Second))
	errored.Error = "permission denied for schema sage"
	assertHealth(t, errored, ago(time.Hour), 0,
		[]string{"error: permission denied for schema sage"})
}

func TestHealthOf_ZeroStatus(t *testing.T) {
	assertHealth(t, &InstanceStatus{}, time.Time{}, 0, []string{"disconnected"})
}

func TestFormatHealthAge(t *testing.T) {
	cases := map[time.Duration]string{
		0:                               "0s",
		59 * time.Second:                "59s",
		time.Minute:                     "1m",
		59*time.Minute + 59*time.Second: "59m",
		time.Hour:                       "1h",
		47 * time.Hour:                  "47h",
		48 * time.Hour:                  "2d",
		-5 * time.Second:                "0s",
	}
	for d, want := range cases {
		if got := formatHealthAge(d); got != want {
			t.Errorf("formatHealthAge(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestFleetStatus_HealthReasonsAndStopReason(t *testing.T) {
	mgr := healthManager()
	s := freshStatus()
	s.FindingsWarning = 5
	registerHealthDB(mgr, "warned", s)
	registerHealthDB(mgr, "stopped", freshStatus())
	mgr.persistStop = func(context.Context, *DatabaseInstance, bool, string) error {
		return nil
	}
	mgr.cancelInFlight = func(*DatabaseInstance) ([]int64, error) { return nil, nil }
	if _, err := mgr.EmergencyStopStrict("stopped", "alice"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	o := mgr.FleetStatus()
	warned := rowByName(t, o, "warned").Status
	if warned.HealthScore != 75 ||
		!reflect.DeepEqual(warned.HealthReasons, []string{"5 warnings"}) {
		t.Fatalf("warned: score %d reasons %q", warned.HealthScore, warned.HealthReasons)
	}
	stopped := rowByName(t, o, "stopped").Status
	if stopped.HealthScore != 100 {
		t.Fatalf("stopped score %d, want 100: a stop carries no penalty",
			stopped.HealthScore)
	}
	if !reflect.DeepEqual(stopped.HealthReasons, []string{"emergency stopped by alice"}) {
		t.Fatalf("stopped reasons %q", stopped.HealthReasons)
	}
}

func TestStopHealthReason(t *testing.T) {
	if got := stopHealthReason("alice"); got != "emergency stopped by alice" {
		t.Fatalf("got %q", got)
	}
	if got := stopHealthReason(""); got != "emergency stopped" {
		t.Fatalf("empty actor: got %q", got)
	}
}

func TestRegisterInstance_StampsRegisteredAt(t *testing.T) {
	mgr := healthManager()
	before := time.Now()
	inst := registerHealthDB(mgr, "new", freshStatus())
	if inst.RegisteredAt.Before(before) || inst.RegisteredAt.After(time.Now()) {
		t.Fatalf("RegisteredAt %v not stamped at registration (after %v)",
			inst.RegisteredAt, before)
	}
	preset := ago(time.Hour)
	kept := &DatabaseInstance{Name: "kept", RegisteredAt: preset,
		Status: &InstanceStatus{Connected: true}}
	mgr.RegisterInstance(kept)
	if !kept.RegisteredAt.Equal(preset) {
		t.Fatalf("preset RegisteredAt overwritten: %v", kept.RegisteredAt)
	}
}

func TestPublishRegistration_StampsRegisteredAt(t *testing.T) {
	mgr := healthManager()
	inst := &DatabaseInstance{Name: "published", Status: &InstanceStatus{}}
	err := mgr.WithLifecycle(context.Background(), func(op *LifecycleMutation) error {
		return op.PublishRegistration(inst)
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if inst.RegisteredAt.IsZero() {
		t.Fatal("PublishRegistration left RegisteredAt zero")
	}
}

func TestFleetStatus_NeverSampledOldInstanceIsStale(t *testing.T) {
	mgr := healthManager()
	s := freshStatus()
	s.CollectorLastRun = time.Time{}
	old := &DatabaseInstance{Name: "old", RegisteredAt: time.Now().Add(-time.Hour),
		Status: &s}
	mgr.RegisterInstance(old)
	registerHealthDB(mgr, "new", s)
	o := mgr.FleetStatus()
	oldRow := rowByName(t, o, "old").Status
	if oldRow.HealthScore != 25 ||
		!strings.HasPrefix(strings.Join(oldRow.HealthReasons, ","), "collector stale") {
		t.Fatalf("old: score %d reasons %q", oldRow.HealthScore, oldRow.HealthReasons)
	}
	if got := rowByName(t, o, "new").Status.HealthScore; got != 100 {
		t.Fatalf("new never-sampled score %d, want 100", got)
	}
}

func TestInstanceStatus_MatchesFleetRow(t *testing.T) {
	mgr := healthManager()
	s := freshStatus()
	s.FindingsCritical = 1
	s.CollectorLastRun = time.Now().Add(-time.Hour)
	registerHealthDB(mgr, "x", s)
	ds, ok := mgr.InstanceStatus("x")
	if !ok {
		t.Fatal("InstanceStatus(x) not found")
	}
	row := rowByName(t, mgr.FleetStatus(), "x")
	if ds.Status.HealthScore != 25 || ds.Status.HealthScore != row.Status.HealthScore {
		t.Fatalf("instance score %d, fleet row %d, want 25 for both",
			ds.Status.HealthScore, row.Status.HealthScore)
	}
	if !reflect.DeepEqual(ds.Status.HealthReasons, row.Status.HealthReasons) {
		t.Fatalf("reasons differ: %q vs %q", ds.Status.HealthReasons,
			row.Status.HealthReasons)
	}
	if ds.Name != "x" || ds.Status.DatabaseName != "x" {
		t.Fatalf("name %q database_name %q", ds.Name, ds.Status.DatabaseName)
	}
}

func TestInstanceStatus_Missing(t *testing.T) {
	mgr := healthManager()
	if _, ok := mgr.InstanceStatus("nope"); ok {
		t.Fatal("empty manager reported an instance")
	}
	registerHealthDB(mgr, "x", freshStatus())
	if _, ok := mgr.InstanceStatus(""); ok {
		t.Fatal("empty name resolved to an instance")
	}
}

// The snapshot is a copy: writers keep running while readers score it.
func TestInstanceStatus_ConcurrentWriters(t *testing.T) {
	mgr := healthManager()
	inst := registerHealthDB(mgr, "x", freshStatus())
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				inst.UpdateStatus(func(s *InstanceStatus) {
					s.FindingsWarning = (i + w) % 3
					s.CollectorLastRun = time.Now()
				})
			}
		}(w)
	}
	for i := 0; i < 200; i++ {
		ds, ok := mgr.InstanceStatus("x")
		if !ok {
			t.Fatal("instance vanished")
		}
		if ds.Status.HealthScore < 90 || ds.Status.HealthScore > 100 {
			t.Fatalf("score %d outside 90-100 for 0-2 warnings", ds.Status.HealthScore)
		}
	}
	wg.Wait()
}
