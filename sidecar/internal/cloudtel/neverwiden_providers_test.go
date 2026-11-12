package cloudtel

import (
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Never-widen property for the Azure and AlloyDB mappings: 2000 random
// provider readings (stale, future, negative, NaN, above 100%) go through
// the sample mapping, then through verify.DecideAdmission. Whenever the
// mapped CPU is not usable, admission is never more permissive than with
// CPU unknown; host withhold reasons never turn a refusal into an
// admission; and an invalid reading never becomes 0%.

func randomReading(rng *rand.Rand, now time.Time) *Point {
	if rng.Intn(5) == 0 {
		return nil
	}
	values := []float64{rng.Float64() * 100, rng.Float64() * 120, -rng.Float64(),
		math.NaN(), math.Inf(1), 100, 100.01, 0}
	ages := []time.Duration{0, time.Minute, MaxPointAge, MaxPointAge + time.Second,
		time.Hour, -MaxClockSkew - time.Minute}
	return &Point{Value: values[rng.Intn(len(values))], At: now.Add(-ages[rng.Intn(len(ages))])}
}

func randomAzureSample(rng *rand.Rand, now time.Time) Sample {
	primary := map[string]*Point{}
	for _, m := range azurePrimaryMetrics {
		primary[m] = randomReading(rng, now)
	}
	if p := primary["storage_used"]; p != nil {
		p.Value *= gibF
	}
	skus := []string{"Standard_D4ds_v4", "Standard_B1ms", "Standard_unknown", ""}
	r := azureReadings{sku: skus[rng.Intn(len(skus))], autoGrow: rng.Intn(3) == 0,
		primary: primary, lag: randomReading(rng, now), replicas: rng.Intn(3)}
	return r.sample("azure:x", now)
}

func randomAlloyDBSample(rng *rand.Rand, now time.Time) Sample {
	values := map[string]*Point{}
	for _, m := range alloyDBMetrics {
		values[m] = randomReading(rng, now)
	}
	if p := values["cpu/vcpus"]; p != nil && rng.Intn(2) == 0 {
		p.Value = 4
	}
	return alloyDBSample("alloydb:x", values, now)
}

func TestProviderMappingsNeverWidenAdmission(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	opts := verify.DefaultOptions()
	now := azureNow
	for i := 0; i < 2000; i++ {
		var s Sample
		if i%2 == 0 {
			s = randomAzureSample(rng, now)
		} else {
			s = randomAlloyDBSample(rng, now)
		}
		base := verify.LoadEvidence{WindowOpen: rng.Intn(2) == 0,
			Rate:     &verify.IORate{DataBytesPerSec: rng.Float64() * 2e6, Interval: time.Minute},
			Capacity: &verify.IOCapacity{ReadWriteMBps: 1, WALMBps: 1}}
		unknown := verify.DecideAdmission(base, opts)
		cpu, cpuErr := s.CPU(now)
		with := base
		if cpuErr == nil {
			with.CPUPct = &cpu
			if cpu < 0 || cpu > 100 || math.IsNaN(cpu) {
				t.Fatalf("CPU() returned invalid %v for %+v", cpu, s.CPUPct)
			}
		} else if s.CPUPct != nil && s.CPUPct.fresh(now) && s.CPUPct.Value <= 100 {
			t.Fatalf("usable CPU %+v was dropped: %v", s.CPUPct, cpuErr)
		}
		plain := verify.DecideAdmission(with, opts)
		with.HostWithhold = strings.Join(Withhold(s, Runway{}, now, DefaultLimits()), "; ")
		got := verify.DecideAdmission(with, opts)
		if got.OK && !plain.OK {
			t.Fatalf("withhold reasons widened admission: %+v", s)
		}
		if cpuErr != nil && got.OK && !unknown.OK {
			t.Fatalf("unusable CPU admitted more than unknown CPU: %+v", s.CPUPct)
		}
		if total, avail := s.HostMemory(now); avail > total {
			t.Fatalf("available %d above total %d", avail, total)
		}
	}
}
