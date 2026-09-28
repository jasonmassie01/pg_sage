package providerobs

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

func scrape(at time.Time, idle float64) string {
	return fmt.Sprintf("node_time_seconds %d\n"+
		"node_cpu_seconds_total{cpu=\"0\",mode=\"idle\"} %g\n"+
		"node_cpu_seconds_total{cpu=\"1\",mode=\"idle\"} %g\n"+
		"node_memory_MemTotal_bytes 1000\nnode_memory_MemAvailable_bytes 250\n",
		at.Unix(), idle, idle)
}

func TestRuntimeTelemetryPreservesUnknownAndCounterUnits(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	first, err := ParseMetrics(scrape(now.Add(-time.Minute), 100), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseMetrics(scrape(now, 145), now)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := DeriveTelemetry(first, second, now)
	if err != nil {
		t.Fatal(err)
	}
	if sample.CPUPct == nil || *sample.CPUPct != 25 || sample.MemoryAvailableBytes == nil ||
		*sample.MemoryAvailableBytes != 250 {
		t.Fatalf("sample = %#v", sample)
	}
	// Only CPU crosses the admission boundary: provider disk counters are
	// not utilization, so Telemetry carries no IO percentage at all.
	cpu, err := sample.CPU(now)
	if err != nil || cpu != 25 {
		t.Fatalf("CPU evidence = %v, %v; want 25", cpu, err)
	}
	if _, err := (Telemetry{ObservedAt: now}).CPU(now); !errors.Is(
		err, verify.ErrLoadTelemetryUnavailable) {
		t.Fatalf("missing CPU admitted: %v", err)
	}
}

func TestRuntimeTelemetryRejectsStaleResetAndMissing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, body := range []string{"", "node_time_seconds NaN", "node_time_seconds -1",
		scrape(now.Add(-10*time.Minute), 100), scrape(now.Add(time.Minute), 100)} {
		if _, err := ParseMetrics(body, now); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	first, _ := ParseMetrics(scrape(now.Add(-time.Minute), 100), now.Add(-time.Minute))
	for _, idle := range []float64{99, 161} {
		second, _ := ParseMetrics(scrape(now, idle), now)
		if _, err := DeriveTelemetry(first, second, now); err == nil {
			t.Errorf("accepted reset or impossible idle %g", idle)
		}
	}
}

func TestAdmissionTelemetryValidatesCPUAndAge(t *testing.T) {
	now := time.Now()
	zero := 0.0
	s := Telemetry{ObservedAt: now, CPUPct: &zero}
	cpu, err := s.CPU(now)
	if err != nil || cpu != 0 {
		t.Fatalf("measured zero = %v %v", cpu, err)
	}
	for _, invalid := range []float64{-1, 101, math.NaN(), math.Inf(1)} {
		s.CPUPct = &invalid
		if _, err := s.CPU(now); !errors.Is(err, verify.ErrLoadTelemetryUnavailable) {
			t.Errorf("accepted %g: %v", invalid, err)
		}
	}
	s.CPUPct = &zero
	if _, err := s.CPU(now.Add(3 * time.Minute)); err == nil {
		t.Fatal("accepted stale telemetry")
	}
	if _, err := s.CPU(now.Add(-time.Minute)); err == nil {
		t.Fatal("accepted future telemetry")
	}
}
