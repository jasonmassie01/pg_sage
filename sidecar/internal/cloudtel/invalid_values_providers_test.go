package cloudtel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// clouds-09 review: an invalid CPU reading is dropped at the mapping with
// a reason, not only refused later by Sample.CPU, so status and findings
// never show it; and an ARM replicas list that names the server itself
// adds no replica (the primary is not its own replica).
//
// No concurrent access tests: each case collects once from its own fake.

func TestAzureInvalidCPUIsDroppedWithAReason(t *testing.T) {
	for _, v := range []float64{100.01, -1} {
		f := newFakeARM(t).healthy()
		f.metrics["orders"]["cpu_percent"] = []agoPoint{{value: v, ago: time.Minute}}
		s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
		if err != nil {
			t.Fatal(err)
		}
		if s.CPUPct != nil || !missingMentions(s, "cpu: invalid utilization") {
			t.Fatalf("cpu %v: CPUPct=%+v missing=%v, want nil with the reason", v, s.CPUPct,
				s.Missing)
		}
	}
}

func TestAlloyDBInvalidCPUIsDroppedWithAReason(t *testing.T) {
	f := newFakeAlloyDB(t).healthy()
	f.series["cpu/average_utilization"] = []alloySeries{{
		points: []agoPoint{{value: 100.01, ago: time.Minute}}}}
	s, err := f.source(t, nil).Collect(context.Background(), alloyNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct != nil || !missingMentions(s, "cpu: invalid utilization") {
		t.Fatalf("CPUPct=%+v missing=%v, want nil with the reason", s.CPUPct, s.Missing)
	}
}

func TestAzureReplicaListNamingTheServerItselfAddsNoReplica(t *testing.T) {
	f := newFakeARM(t).healthy()
	f.replicas = []string{"orders"} // ARM may list the primary among its replicas
	s, err := f.source(t, nil, nil).Collect(context.Background(), azureNow)
	if err != nil {
		t.Fatal(err)
	}
	if s.ReplicaLagSeconds != nil || missingMentions(s, "replica_lag") {
		t.Fatalf("lag=%+v missing=%v: a server without replicas has no lag to miss",
			s.ReplicaLagSeconds, s.Missing)
	}
	metricsReads := 0
	for _, p := range f.paths {
		if strings.Contains(p, "/Microsoft.Insights/metrics") {
			metricsReads++
		}
	}
	if metricsReads != 1 {
		t.Fatalf("metrics reads = %d (%v), want only the server's own", metricsReads, f.paths)
	}
}
