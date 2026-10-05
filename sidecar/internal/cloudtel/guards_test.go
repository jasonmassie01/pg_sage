package cloudtel

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

var guardNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func pt(v float64, ago time.Duration) *Point { return &Point{Value: v, At: guardNow.Add(-ago)} }

func TestSampleCPU(t *testing.T) {
	cases := []struct {
		name string
		p    *Point
		want float64
		ok   bool
	}{
		{"fresh", pt(37.5, time.Minute), 37.5, true},
		{"zero is a measurement", pt(0, time.Minute), 0, true},
		{"hundred", pt(100, time.Minute), 100, true},
		{"at max age", pt(10, MaxPointAge), 10, true},
		{"stale", pt(10, MaxPointAge+time.Second), 0, false},
		{"future beyond skew", pt(10, -MaxClockSkew-time.Second), 0, false},
		{"over 100", pt(100.5, time.Minute), 0, false},
		{"negative", pt(-1, time.Minute), 0, false},
		{"nan", pt(math.NaN(), time.Minute), 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Sample{CPUPct: tc.p}.CPU(guardNow)
			if tc.ok != (err == nil) || got != tc.want {
				t.Fatalf("CPU = %v, %v; want %v ok=%t", got, err, tc.want, tc.ok)
			}
			if err != nil && !errors.Is(err, verify.ErrLoadTelemetryUnavailable) {
				t.Fatalf("CPU error %v must wrap verify.ErrLoadTelemetryUnavailable", err)
			}
		})
	}
}

func TestSampleHostMemory(t *testing.T) {
	s := Sample{MemoryTotalBytes: pt(16*gib, time.Minute),
		FreeableMemoryBytes: pt(4*gib, time.Minute)}
	total, avail := s.HostMemory(guardNow)
	if total != int64(16*gib) || avail != int64(4*gib) {
		t.Fatalf("HostMemory = %d, %d", total, avail)
	}
	stale := Sample{MemoryTotalBytes: pt(16*gib, time.Hour)}
	if total, _ := stale.HostMemory(guardNow); total != 0 {
		t.Fatalf("stale memory total = %d, want 0", total)
	}
	bad := Sample{MemoryTotalBytes: pt(16*gib, time.Minute),
		FreeableMemoryBytes: pt(20*gib, time.Minute)}
	if total, avail := bad.HostMemory(guardNow); total != int64(16*gib) || avail != 0 {
		t.Fatalf("available above total must be dropped: %d, %d", total, avail)
	}
	if total, avail := (Sample{}).HostMemory(guardNow); total != 0 || avail != 0 {
		t.Fatalf("empty sample = %d, %d", total, avail)
	}
}

func TestEffectiveFreeStorage(t *testing.T) {
	s := Sample{FreeStorageBytes: pt(30*gib, time.Minute),
		AllocatedStorageBytes: pt(100*gib, time.Minute),
		StorageCapacityBytes:  pt(500*gib, time.Minute)}
	free, capacity, ok := s.EffectiveFreeStorage(guardNow)
	if !ok || free != 430*gib || capacity != 500*gib {
		t.Fatalf("autoscaling headroom: free=%v cap=%v ok=%t", free, capacity, ok)
	}
	grows := s
	grows.StorageAutoGrows = true
	if _, _, ok := grows.EffectiveFreeStorage(guardNow); ok {
		t.Fatal("auto-growing storage has no bounded free space")
	}
	if _, _, ok := (Sample{FreeStorageBytes: pt(1, time.Minute)}).EffectiveFreeStorage(
		guardNow); ok {
		t.Fatal("free storage without capacity is not bounded")
	}
}

func TestWithholdBoundaries(t *testing.T) {
	l := DefaultLimits()
	if l.MaxReplicaLagSeconds != 30 || l.MinFreeStoragePct != 10 ||
		l.MinStorageRunwayHours != 24 || l.MinAvailableMemoryPct != 5 {
		t.Fatalf("defaults = %+v", l)
	}
	storage := func(freePct float64) Sample {
		return Sample{FreeStorageBytes: pt(freePct*gib, time.Minute),
			AllocatedStorageBytes: pt(100*gib, time.Minute),
			StorageCapacityBytes:  pt(100*gib, time.Minute)}
	}
	cases := []struct {
		name   string
		s      Sample
		runway Runway
		want   string
	}{
		{"lag at limit", Sample{ReplicaLagSeconds: pt(30, time.Minute)}, Runway{}, ""},
		{"lag above limit", Sample{ReplicaLagSeconds: pt(30.5, time.Minute)}, Runway{},
			"replica lag"},
		{"stale lag ignored", Sample{ReplicaLagSeconds: pt(999, time.Hour)}, Runway{}, ""},
		{"free at floor", storage(10), Runway{}, ""},
		{"free below floor", storage(9.9), Runway{}, "free storage"},
		{"runway at floor", storage(50), Runway{Known: true, Hours: 24}, ""},
		{"runway below floor", storage(50), Runway{Known: true, Hours: 23.9},
			"storage runs out"},
		{"unknown runway", storage(50), Runway{Known: false, Hours: 1}, ""},
		{"memory at floor", Sample{MemoryTotalBytes: pt(100, time.Minute),
			FreeableMemoryBytes: pt(5, time.Minute)}, Runway{}, ""},
		{"memory below floor", Sample{MemoryTotalBytes: pt(100, time.Minute),
			FreeableMemoryBytes: pt(4.9, time.Minute)}, Runway{}, "available memory"},
		{"nothing known", Sample{}, Runway{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Withhold(tc.s, tc.runway, guardNow, l)
			if tc.want == "" && len(got) != 0 {
				t.Fatalf("Withhold = %v, want none", got)
			}
			if tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)) {
				t.Fatalf("Withhold = %v, want one reason containing %q", got, tc.want)
			}
		})
	}
}

// A zero limit disables that guard; it never makes a guard stricter.
func TestWithholdZeroLimitsDisable(t *testing.T) {
	s := Sample{ReplicaLagSeconds: pt(1e6, time.Minute)}
	if got := Withhold(s, Runway{}, guardNow, Limits{}); len(got) != 0 {
		t.Fatalf("zero limits withheld: %v", got)
	}
}

func TestStorageRunway(t *testing.T) {
	var falling []Point
	for i := 0; i < 12; i++ { // 10 GiB free falling 1 GiB per hour, 5-min samples
		ago := time.Duration(11-i) * 5 * time.Minute
		falling = append(falling, Point{Value: 10*gib + float64(55-5*i)/60*gib,
			At: guardNow.Add(-ago)})
	}
	r := StorageRunway(falling, guardNow)
	if !r.Known || math.Abs(r.SlopeBytesPerHour+gib) > gib*0.01 ||
		math.Abs(r.Hours-10) > 0.1 || r.Points != 12 {
		t.Fatalf("falling runway = %+v, want ~10h at -1GiB/h", r)
	}
	var flat []Point
	for i := 0; i < 8; i++ {
		flat = append(flat, Point{Value: 5 * gib, At: guardNow.Add(-time.Duration(i) *
			10 * time.Minute)})
	}
	if r := StorageRunway(flat, guardNow); !r.Known || !math.IsInf(r.Hours, 1) {
		t.Fatalf("flat runway = %+v, want +Inf", r)
	}
	if r := StorageRunway(falling[:5], guardNow); r.Known {
		t.Fatalf("five points must not be enough: %+v", r)
	}
	short := []Point{}
	for i := 0; i < 10; i++ { // 10 points within 9 minutes: span too short
		short = append(short, Point{Value: float64(100 - i), At: guardNow.Add(
			-time.Duration(i) * time.Minute)})
	}
	if r := StorageRunway(short, guardNow); r.Known {
		t.Fatalf("a 9-minute span must not be enough: %+v", r)
	}
	if r := StorageRunway(nil, guardNow); r.Known {
		t.Fatal("empty history must be unknown")
	}
	old := make([]Point, len(falling))
	for i, p := range falling {
		old[i] = Point{Value: p.Value, At: p.At.Add(-48 * time.Hour)}
	}
	if r := StorageRunway(old, guardNow); r.Known {
		t.Fatalf("history older than 24h must be ignored: %+v", r)
	}
}

// Never-widen property: telemetry only ever adds withhold reasons. For any
// sample, admission with the cloud reasons is admitted only if admission
// without them is admitted, and the reasons never touch CPU or capacity.
func TestWithholdNeverWidensAdmission(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	opts := verify.DefaultOptions()
	for i := 0; i < 2000; i++ {
		s := randomSample(rng)
		runway := Runway{Known: rng.Intn(2) == 0, Hours: rng.Float64() * 100}
		reasons := Withhold(s, runway, guardNow, DefaultLimits())
		cpu := rng.Float64() * 100
		base := verify.LoadEvidence{CPUPct: &cpu, WindowOpen: rng.Intn(2) == 0,
			Rate:     &verify.IORate{DataBytesPerSec: rng.Float64() * 2e6, Interval: time.Minute},
			Capacity: &verify.IOCapacity{ReadWriteMBps: 1, WALMBps: 1}}
		with := base
		with.HostWithhold = reasons
		a, b := verify.DecideAdmission(base, opts), verify.DecideAdmission(with, opts)
		if b.OK && !a.OK {
			t.Fatalf("telemetry widened admission: sample %+v reasons %v", s, reasons)
		}
		if len(reasons) > 0 && b.OK {
			t.Fatalf("withhold reasons %v did not withhold", reasons)
		}
	}
}

func randomSample(rng *rand.Rand) Sample {
	maybe := func(v float64) *Point {
		if rng.Intn(3) == 0 {
			return nil
		}
		return pt(v, time.Duration(rng.Intn(20))*time.Minute)
	}
	return Sample{ReplicaLagSeconds: maybe(rng.Float64() * 120),
		FreeStorageBytes:      maybe(rng.Float64() * 100 * gib),
		AllocatedStorageBytes: maybe(100 * gib), StorageCapacityBytes: maybe(100 * gib),
		MemoryTotalBytes: maybe(16 * gib), FreeableMemoryBytes: maybe(rng.Float64() * 16 * gib),
		StorageAutoGrows: rng.Intn(4) == 0}
}

func TestLimitsValidate(t *testing.T) {
	if err := DefaultLimits().Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
	for _, l := range []Limits{{MaxReplicaLagSeconds: -1}, {MinFreeStoragePct: 101},
		{MinStorageRunwayHours: math.NaN()}, {MinAvailableMemoryPct: -0.5}} {
		if err := l.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want error", l)
		}
	}
}
