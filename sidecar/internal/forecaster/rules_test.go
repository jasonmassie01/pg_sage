package forecaster

import (
	"testing"
	"time"
)

// --- disk growth ---

func TestForecastDiskGrowth_Growing(t *testing.T) {
	aggs := makeSysAggs(14, func(i int) DaySystemAgg {
		return DaySystemAgg{
			MaxDBSizeBytes: float64(100e9) + float64(i)*10e9,
		}
	})
	cfg := ForecasterConfig{DiskWarnGrowthGBDay: 5}
	findings := forecastDiskGrowth(aggs, cfg)
	if len(findings) == 0 {
		t.Fatal("expected disk growth finding")
	}
	if findings[0].Category != "forecast_disk_growth" {
		t.Errorf("category = %q", findings[0].Category)
	}
}

func TestForecastDiskGrowth_Flat(t *testing.T) {
	aggs := makeSysAggs(14, func(_ int) DaySystemAgg {
		return DaySystemAgg{MaxDBSizeBytes: 100e9}
	})
	cfg := ForecasterConfig{DiskWarnGrowthGBDay: 5}
	findings := forecastDiskGrowth(aggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for flat data")
	}
}

func TestForecastDiskGrowth_InsufficientData(t *testing.T) {
	aggs := makeSysAggs(3, func(_ int) DaySystemAgg {
		return DaySystemAgg{MaxDBSizeBytes: 100e9}
	})
	cfg := ForecasterConfig{DiskWarnGrowthGBDay: 5}
	findings := forecastDiskGrowth(aggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for insufficient data")
	}
}

// --- connection saturation ---

func TestForecastConnectionSaturation_Growing(t *testing.T) {
	aggs := makeSysAggs(30, func(i int) DaySystemAgg {
		return DaySystemAgg{
			MaxActiveBackends: float64(50 + i*3),
			MaxConnections:    200,
		}
	})
	cfg := ForecasterConfig{ConnectionWarnPct: 80}
	findings := forecastConnectionSaturation(aggs, cfg)
	if len(findings) == 0 {
		t.Fatal("expected connection saturation finding")
	}
	cat := findings[0].Category
	if cat != "forecast_connection_saturation" {
		t.Errorf("category = %q", cat)
	}
}

func TestForecastConnectionSaturation_Flat(t *testing.T) {
	aggs := makeSysAggs(30, func(_ int) DaySystemAgg {
		return DaySystemAgg{
			MaxActiveBackends: 20,
			MaxConnections:    200,
		}
	})
	cfg := ForecasterConfig{ConnectionWarnPct: 80}
	findings := forecastConnectionSaturation(aggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for flat connections")
	}
}

func TestForecastConnectionSaturation_Insufficient(t *testing.T) {
	aggs := makeSysAggs(3, func(_ int) DaySystemAgg {
		return DaySystemAgg{
			MaxActiveBackends: 50,
			MaxConnections:    200,
		}
	})
	cfg := ForecasterConfig{ConnectionWarnPct: 80}
	findings := forecastConnectionSaturation(aggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for insufficient data")
	}
}

// --- cache pressure ---

func TestForecastCachePressure_Declining(t *testing.T) {
	aggs := makeSysAggs(14, func(i int) DaySystemAgg {
		return DaySystemAgg{
			CacheHitRatio: 99.0 - float64(i)*1.5,
		}
	})
	cfg := ForecasterConfig{CacheWarnThreshold: 0.90}
	findings := forecastCachePressure(aggs, cfg)
	if len(findings) == 0 {
		t.Fatal("expected cache pressure finding")
	}
}

func TestForecastCachePressure_Stable(t *testing.T) {
	aggs := makeSysAggs(14, func(_ int) DaySystemAgg {
		return DaySystemAgg{CacheHitRatio: 99.5}
	})
	cfg := ForecasterConfig{CacheWarnThreshold: 0.90}
	findings := forecastCachePressure(aggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for stable cache")
	}
}

func TestForecastCachePressure_Insufficient(t *testing.T) {
	aggs := makeSysAggs(3, func(_ int) DaySystemAgg {
		return DaySystemAgg{CacheHitRatio: 80}
	})
	cfg := ForecasterConfig{CacheWarnThreshold: 0.90}
	findings := forecastCachePressure(aggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for insufficient data")
	}
}

// --- sequence exhaustion ---

// --- query volume ---

func TestForecastQueryVolume_HighGrowth(t *testing.T) {
	qAggs := makeQueryAggs(14, func(i int) float64 {
		if i < 7 {
			return 1000
		}
		return 2500 // 150% growth
	})
	cfg := ForecasterConfig{}
	findings := forecastQueryVolume(qAggs, cfg)
	if len(findings) == 0 {
		t.Fatal("expected query volume finding")
	}
	if findings[0].Severity != "critical" {
		t.Errorf("severity = %q, want critical", findings[0].Severity)
	}
}

func TestForecastQueryVolume_ModerateGrowth(t *testing.T) {
	qAggs := makeQueryAggs(14, func(i int) float64 {
		if i < 7 {
			return 1000
		}
		return 1600 // 60% growth
	})
	cfg := ForecasterConfig{}
	findings := forecastQueryVolume(qAggs, cfg)
	if len(findings) == 0 {
		t.Fatal("expected query volume finding")
	}
	if findings[0].Severity != "warning" {
		t.Errorf("severity = %q, want warning", findings[0].Severity)
	}
}

func TestForecastQueryVolume_Flat(t *testing.T) {
	qAggs := makeQueryAggs(14, func(_ int) float64 {
		return 1000
	})
	cfg := ForecasterConfig{}
	findings := forecastQueryVolume(qAggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for flat volume")
	}
}

func TestForecastQueryVolume_Insufficient(t *testing.T) {
	qAggs := makeQueryAggs(7, func(_ int) float64 {
		return 1000
	})
	cfg := ForecasterConfig{}
	findings := forecastQueryVolume(qAggs, cfg)
	if len(findings) != 0 {
		t.Error("expected no findings for insufficient data")
	}
}

// --- checkpoint pressure ---

// forecast-03 replaced the fixtures below: they gave only a total count
// (10 a day is impossible: the timed counter alone adds 288 a day at the
// default checkpoint_timeout), and the rule now reads the requested count.

func TestForecastCheckpointPressure_High(t *testing.T) {
	aggs := ckptHistory(10, 288, 120) // 5 requested per hour
	findings := forecastCheckpointPressure(aggs, ForecasterConfig{})
	if len(findings) == 0 {
		t.Fatal("expected checkpoint pressure finding")
	}
	cat := findings[0].Category
	if cat != "forecast_checkpoint_pressure" {
		t.Errorf("category = %q", cat)
	}
}

func TestForecastCheckpointPressure_Low(t *testing.T) {
	aggs := ckptHistory(10, 288, 0)
	findings := forecastCheckpointPressure(aggs, ForecasterConfig{})
	if len(findings) != 0 {
		t.Error("expected no findings for timed-only checkpoints")
	}
}

func TestForecastCheckpointPressure_Insufficient(t *testing.T) {
	aggs := ckptHistory(3, 288, 1000)
	findings := forecastCheckpointPressure(aggs, ForecasterConfig{})
	if len(findings) != 0 {
		t.Error("expected no findings for insufficient data")
	}
}

// --- test helpers ---

func makeSysAggs(
	n int, fill func(int) DaySystemAgg,
) []DaySystemAgg {
	aggs := make([]DaySystemAgg, n)
	for i := range aggs {
		aggs[i] = fill(i)
		aggs[i].Day = time.Now().AddDate(0, 0, -n+i)
	}
	return aggs
}

func makeQueryAggs(
	n int, fill func(int) float64,
) []DayQueryAgg {
	aggs := make([]DayQueryAgg, n)
	for i := range aggs {
		aggs[i] = DayQueryAgg{
			Day:        time.Now().AddDate(0, 0, -n+i),
			TotalCalls: fill(i),
		}
	}
	return aggs
}
