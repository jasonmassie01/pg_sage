package main

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

var instanceHealthLine = regexp.MustCompile(
	`(?m)^pg_sage_database_health_score\{database="([^"]+)"\} (\d+)$`)

func scrapeInstanceHealth(t *testing.T) []string {
	t.Helper()
	var b strings.Builder
	writeDatabaseHealthMetrics(&b, fleetMgr)
	var out []string
	for _, m := range instanceHealthLine.FindAllStringSubmatch(b.String(), -1) {
		out = append(out, m[1]+"="+m[2])
	}
	return out
}

// CHECK-12: consecutive scrapes list the per-database health score in one
// order (by name; pg_sage_fleet_instance_health, listed worst first, left
// in v2.6.0), and pg_sage_fleet_healthy excludes a connected database
// whose score is below 50.
func TestFleetMetrics_InstanceHealthOrderIsStable(t *testing.T) {
	prev := fleetMgr
	t.Cleanup(func() { fleetMgr = prev })
	fleetMgr = fleet.NewManager(&config.Config{
		Mode:      "fleet",
		Collector: config.CollectorConfig{IntervalSeconds: 60},
	})
	for _, name := range []string{"delta", "alpha", "charlie", "bravo"} {
		s := &fleet.InstanceStatus{Connected: true, CollectorLastRun: time.Now()}
		if name == "charlie" {
			s.FindingsCritical = 4
		}
		fleetMgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, Config: config.DatabaseConfig{Name: name}, Status: s,
		})
	}
	want := []string{"alpha=100", "bravo=100", "charlie=0", "delta=100"}
	for i := 0; i < 20; i++ {
		got := scrapeInstanceHealth(t)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("scrape %d: %v, want %v", i, got, want)
		}
	}
	var b strings.Builder
	writeFleetMetrics(&b)
	if !strings.Contains(b.String(), "\npg_sage_fleet_healthy 3\n") {
		t.Fatalf("pg_sage_fleet_healthy should be 3:\n%s", b.String())
	}
}
