package forecaster

import (
	"context"
	"math"
	"testing"

	"github.com/pg-sage/sidecar/internal/runway"
)

// forecast-06 (review): Forecast itself, not only withDiskCapacity, puts
// the run's capacity on forecast_disk_growth. The seed is 7 measured days
// growing 6 GB/day to 142 GB (seedNullSizeDay).

// No concurrent access tests here: TestDiskCapacity_SourceReadWhileTelemetryRecords
// covers the source read under concurrent writes.

// forecastDiskDetail runs Forecast on the seed and returns the disk detail.
func forecastDiskDetail(t *testing.T, f *Forecaster, ctx context.Context) map[string]any {
	t.Helper()
	findings, err := f.Forecast(ctx)
	if err != nil {
		t.Fatalf("Forecast: %v", err)
	}
	for _, fd := range findings {
		if fd.Category == "forecast_disk_growth" {
			return fd.Detail
		}
	}
	t.Fatalf("no forecast_disk_growth finding: %+v", findings)
	return nil
}

// diskForecaster is the seed's forecaster config with a declared capacity.
func diskForecaster(declared int64) ForecasterConfig {
	return ForecasterConfig{LookbackDays: 30, DiskWarnGrowthGBDay: 5,
		ConnectionWarnPct: 80, CacheWarnThreshold: 0.5, DiskCapacityBytes: declared}
}

func TestDiskCapacity_ForecastCarriesTheDeclaredTimeToFull(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedNullSizeDay(t, ctx, pool)
	f := New(pool, diskForecaster(202e9), func(string, string, ...any) {})
	disk := forecastDiskDetail(t, f, ctx)
	if disk["capacity_source"] != CapacityDeclared || disk["capacity_bytes"] != 202e9 {
		t.Fatalf("capacity = %v / %v, want declared 202e9",
			disk["capacity_source"], disk["capacity_bytes"])
	}
	days, ok := disk["days_until_full"].(float64)
	if !ok || math.Abs(days-10) > 0.1 {
		t.Fatalf("days_until_full = %v, want about 10 ((202-142) GB / 6 GB/day)",
			disk["days_until_full"])
	}
	if _, named := disk["config_key"]; named {
		t.Errorf("a declared capacity still names the config key: %v", disk)
	}
}

func TestDiskCapacity_ForecastUsesTheProviderSourceNotTheDeclaredValue(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedNullSizeDay(t, ctx, pool)
	fake := &fakeCapacity{}
	fake.set(0, runway.CapacityUnbounded, false)
	f := New(pool, diskForecaster(202e9), func(string, string, ...any) {}).
		WithCapacity(fake.src())
	disk := forecastDiskDetail(t, f, ctx)
	if disk["capacity_source"] != CapacityUnbounded {
		t.Fatalf("capacity_source = %v, want %s", disk["capacity_source"], CapacityUnbounded)
	}
	if _, has := disk["days_until_full"]; has {
		t.Errorf("unbounded storage got a time to full from the declared value: %v", disk)
	}
	if disk["authority_under_7_days"] != managedStorageAuthority {
		t.Errorf("managed database lacks the managed-storage authority: %v", disk)
	}
	if fake.calls == 0 {
		t.Fatal("Forecast never read the provider capacity source")
	}
}

func TestDiskCapacity_ForecastWithoutCapacityIsUndeclared(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedNullSizeDay(t, ctx, pool)
	f := New(pool, diskForecaster(0), func(string, string, ...any) {})
	disk := forecastDiskDetail(t, f, context.Background())
	if disk["capacity_source"] != CapacityUndeclared ||
		disk["config_key"] != "forecaster.disk_capacity_bytes" {
		t.Fatalf("undeclared detail = %v", disk)
	}
	if _, has := disk["days_until_full"]; has {
		t.Errorf("undeclared capacity got a time to full: %v", disk)
	}
}
