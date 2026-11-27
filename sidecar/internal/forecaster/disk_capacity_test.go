package forecaster

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/runway"
)

// forecast-06: forecast_disk_growth carries the disk capacity it was
// judged against (provider telemetry when fresh, else the declared
// forecaster.disk_capacity_bytes), the capacity's source and, when the
// capacity is known, the days until the database fills it.

// utcSizeDays is n closed UTC days ending at endGB, growing gbPerDay: one
// exact day apart, so the fitted slope is exactly gbPerDay.
func utcSizeDays(n int, endGB, gbPerDay float64) []DaySystemAgg {
	origin := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	aggs := make([]DaySystemAgg, n)
	for i := range aggs {
		size := (endGB - float64(n-1-i)*gbPerDay) * 1e9
		aggs[i] = DaySystemAgg{Day: origin.AddDate(0, 0, i),
			AvgDBSizeBytes: size, MaxDBSizeBytes: size}
	}
	return aggs
}

// diskFindingWith is the single disk finding of aggs judged against c.
func diskFindingWith(t *testing.T, aggs []DaySystemAgg, c Capacity) analyzer.Finding {
	t.Helper()
	cfg := ForecasterConfig{DiskWarnGrowthGBDay: 0.5}
	findings := withDiskCapacity(forecastDiskGrowth(aggs, cfg), c)
	if len(findings) != 1 {
		t.Fatalf("want one disk finding, got %d: %+v", len(findings), findings)
	}
	return findings[0]
}

// fakeCapacity is a provider capacity source the test can change.
type fakeCapacity struct {
	mu     sync.Mutex
	bytes  float64
	source string
	ok     bool
	calls  int
}

func (f *fakeCapacity) set(bytes float64, source string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bytes, f.source, f.ok = bytes, source, ok
}

func (f *fakeCapacity) read(context.Context) (float64, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.bytes, f.source, f.ok
}

func (f *fakeCapacity) src() runway.CapacitySource { return f.read }

func TestDiskCapacity_DeclaredGivesDaysUntilFull(t *testing.T) {
	c := ResolveCapacity(context.Background(), 100e9, nil)
	f := diskFindingWith(t, utcSizeDays(8, 70, 1), c)
	d := f.Detail
	if d["capacity_source"] != CapacityDeclared || d["capacity_bytes"] != 100e9 {
		t.Fatalf("detail = %v, want declared capacity 100e9", d)
	}
	days, ok := d["days_until_full"].(float64)
	if !ok || math.Abs(days-30) > 1e-6 {
		t.Fatalf("days_until_full = %v, want 30 ((100-70) GB at 1 GB/day)", d["days_until_full"])
	}
	if d["current_size_bytes"] != 70e9 {
		t.Fatalf("current_size_bytes = %v, want 70e9", d["current_size_bytes"])
	}
	rate, ok := d["growth_rate_bytes_day"].(float64)
	if !ok || math.Abs(rate-1e9) > 1 {
		t.Fatalf("growth_rate_bytes_day = %v, want 1e9", d["growth_rate_bytes_day"])
	}
	if _, set := d["config_key"]; set {
		t.Fatalf("a declared capacity names no config key: %v", d)
	}
	if !strings.Contains(f.Recommendation, "30 days") {
		t.Fatalf("recommendation %q does not give the time to full", f.Recommendation)
	}
}

func TestDiskCapacity_FullOrPastFullIsZeroNeverNegative(t *testing.T) {
	for name, capacity := range map[string]int64{"equal": 70e9, "below": 60e9} {
		t.Run(name, func(t *testing.T) {
			c := ResolveCapacity(context.Background(), capacity, nil)
			d := diskFindingWith(t, utcSizeDays(8, 70, 1), c).Detail
			if days, ok := d["days_until_full"].(float64); !ok || days != 0 {
				t.Fatalf("days_until_full = %v, want 0", d["days_until_full"])
			}
		})
	}
}

func TestDiskCapacity_UndeclaredNamesTheConfigKey(t *testing.T) {
	c := ResolveCapacity(context.Background(), 0, nil)
	if c.Source != CapacityUndeclared || c.Bytes != 0 || c.Managed {
		t.Fatalf("capacity = %+v, want undeclared, 0 bytes, not managed", c)
	}
	f := diskFindingWith(t, utcSizeDays(8, 70, 1), c)
	d := f.Detail
	if d["capacity_source"] != CapacityUndeclared ||
		d["config_key"] != "forecaster.disk_capacity_bytes" {
		t.Fatalf("detail = %v, want undeclared naming forecaster.disk_capacity_bytes", d)
	}
	for _, key := range []string{"days_until_full", "capacity_bytes", "authority_under_7_days"} {
		if _, set := d[key]; set {
			t.Fatalf("undeclared detail sets %s: %v", key, d)
		}
	}
	if !strings.Contains(f.Recommendation, "forecaster.disk_capacity_bytes") {
		t.Fatalf("recommendation %q does not say how to get a time to full",
			f.Recommendation)
	}
}

func TestDiskCapacity_NilSourceUsesTheDeclaredValueOnly(t *testing.T) {
	if c := ResolveCapacity(context.Background(), 0, nil); c.Source != CapacityUndeclared {
		t.Fatalf("nil source, nothing declared = %+v, want undeclared", c)
	}
	c := ResolveCapacity(context.Background(), 5e9, nil)
	if c.Source != CapacityDeclared || c.Bytes != 5e9 || c.Managed {
		t.Fatalf("nil source, 5e9 declared = %+v, want declared 5e9, not managed", c)
	}
	if c := ResolveCapacity(context.Background(), -1, nil); c.Source != CapacityUndeclared {
		t.Fatalf("negative declared = %+v, want undeclared", c)
	}
}

func TestDiskCapacity_ZeroValueCapacityIsUndeclared(t *testing.T) {
	d := diskFindingWith(t, utcSizeDays(8, 70, 1), Capacity{}).Detail
	if d["capacity_source"] != CapacityUndeclared {
		t.Fatalf("zero Capacity detail = %v, want undeclared", d)
	}
	if got := withDiskCapacity(nil, Capacity{}); len(got) != 0 {
		t.Fatalf("no findings in, got %v", got)
	}
}

func TestDiskCapacity_FreshProviderWinsOverDeclared(t *testing.T) {
	fake := &fakeCapacity{}
	fake.set(200e9, runway.CapacityProviderTelemetry, true)
	c := ResolveCapacity(context.Background(), 100e9, fake.src())
	if c.Source != CapacityProviderTelemetry || c.Bytes != 200e9 || !c.Managed {
		t.Fatalf("capacity = %+v, want provider 200e9, managed", c)
	}
	d := diskFindingWith(t, utcSizeDays(8, 70, 1), c).Detail
	if days, _ := d["days_until_full"].(float64); math.Abs(days-130) > 1e-6 {
		t.Fatalf("days_until_full = %v, want 130", d["days_until_full"])
	}
	if d["authority_under_7_days"] != "managed_storage_runway" {
		t.Fatalf("managed detail = %v, want managed_storage_runway as the authority", d)
	}
}

func TestDiskCapacity_UnboundedProviderIsNotReplacedByDeclared(t *testing.T) {
	fake := &fakeCapacity{}
	fake.set(0, runway.CapacityUnbounded, false)
	c := ResolveCapacity(context.Background(), 100e9, fake.src())
	if c.Source != CapacityUnbounded || c.Bytes != 0 || !c.Managed {
		t.Fatalf("capacity = %+v, want unbounded with no bytes (declared not used)", c)
	}
	d := diskFindingWith(t, utcSizeDays(8, 70, 1), c).Detail
	if d["capacity_source"] != CapacityUnbounded {
		t.Fatalf("detail = %v, want capacity_unbounded", d)
	}
	for _, key := range []string{"days_until_full", "capacity_bytes", "config_key"} {
		if _, set := d[key]; set {
			t.Fatalf("unbounded detail sets %s: %v", key, d)
		}
	}
	if fake.calls != 1 {
		t.Fatalf("provider read %d times, want once", fake.calls)
	}
}

func TestDiskCapacity_StaleOrBadProviderFallsBackToDeclared(t *testing.T) {
	cases := map[string]struct {
		bytes float64
		ok    bool
	}{
		"stale":    {0, false},
		"nan":      {math.NaN(), true},
		"infinite": {math.Inf(1), true},
		"negative": {-5, true},
		"zero":     {0, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeCapacity{}
			fake.set(tc.bytes, "", tc.ok)
			c := ResolveCapacity(context.Background(), 100e9, fake.src())
			if c.Source != CapacityDeclared || c.Bytes != 100e9 || !c.Managed {
				t.Fatalf("capacity = %+v, want declared 100e9 on a managed database", c)
			}
			c = ResolveCapacity(context.Background(), 0, fake.src())
			if c.Source != CapacityUndeclared || c.Bytes != 0 {
				t.Fatalf("nothing declared = %+v, want undeclared", c)
			}
		})
	}
}

func TestDiskCapacity_ForecasterReadsItsSourceEachRun(t *testing.T) {
	fake := &fakeCapacity{}
	fake.set(200e9, runway.CapacityProviderTelemetry, true)
	f := New(nil, ForecasterConfig{DiskCapacityBytes: 100e9},
		func(string, string, ...any) {}).WithCapacity(fake.src())
	if c := f.diskCapacity(context.Background()); c.Source != CapacityProviderTelemetry {
		t.Fatalf("first run = %+v, want provider telemetry", c)
	}
	fake.set(0, runway.CapacityUnbounded, false)
	if c := f.diskCapacity(context.Background()); c.Source != CapacityUnbounded {
		t.Fatalf("after the provider turned unbounded = %+v, want capacity_unbounded", c)
	}
	plain := New(nil, ForecasterConfig{DiskCapacityBytes: 100e9}, func(string, string, ...any) {})
	if c := plain.diskCapacity(context.Background()); c.Source != CapacityDeclared {
		t.Fatalf("no source = %+v, want declared", c)
	}
}

// The capacity source is read from the analyzer goroutine while cloud
// telemetry records new samples. -race is unavailable on this machine
// (no cgo); the fake's lock and the result checks still catch a torn read.
func TestDiskCapacity_SourceReadWhileTelemetryRecords(t *testing.T) {
	fake := &fakeCapacity{}
	fake.set(200e9, runway.CapacityProviderTelemetry, true)
	f := New(nil, ForecasterConfig{DiskCapacityBytes: 100e9},
		func(string, string, ...any) {}).WithCapacity(fake.src())
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				fake.set(0, runway.CapacityUnbounded, false)
			} else {
				fake.set(200e9, runway.CapacityProviderTelemetry, true)
			}
		}
	}()
	var readers sync.WaitGroup
	errs := make(chan string, 8)
	for r := 0; r < 8; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 200; i++ {
				c := f.diskCapacity(context.Background())
				valid := c.Source == CapacityProviderTelemetry && c.Bytes == 200e9 ||
					c.Source == CapacityUnbounded && c.Bytes == 0
				if !valid {
					errs <- c.Source
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writer.Wait()
	close(errs)
	for bad := range errs {
		t.Fatalf("torn capacity read: %q", bad)
	}
}
