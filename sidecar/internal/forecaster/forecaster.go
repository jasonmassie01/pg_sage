package forecaster

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/runway"
)

// ForecasterConfig holds thresholds for workload forecasting.
type ForecasterConfig struct {
	Enabled             bool
	LookbackDays        int
	DiskWarnGrowthGBDay float64
	ConnectionWarnPct   float64
	CacheWarnThreshold  float64
	// CacheMinBlocksPerDay is the day's block accesses below which its
	// cache hit ratio is left out (forecast-04); 0 means 100,000.
	CacheMinBlocksPerDay int64
	// CheckpointRequestedPerHour is the requested-checkpoint rate at
	// which the checkpoint forecast warns (forecast-03); 0 means 2.
	CheckpointRequestedPerHour float64
	// DiskCapacityBytes is the declared disk capacity
	// (forecaster.disk_capacity_bytes; 0: undeclared).
	DiskCapacityBytes int64
	MinRSquared       float64
}

// Forecaster produces capacity forecast findings from historical
// snapshot data using statistical methods.
type Forecaster struct {
	pool  *pgxpool.Pool
	cfg   ForecasterConfig
	logFn func(string, string, ...any)
	// capacity is the provider's storage capacity (nil without
	// managed-cloud telemetry); set before the analyzer starts.
	capacity runway.CapacitySource

	mu            sync.Mutex
	lastEvaluated []string
	// history remembers the decoded daily samples across runs.
	history dayHistory
}

// New creates a new Forecaster.
func New(
	pool *pgxpool.Pool,
	cfg ForecasterConfig,
	logFn func(string, string, ...any),
) *Forecaster {
	return &Forecaster{pool: pool, cfg: cfg, logFn: logFn}
}

// WithCapacity sets the provider capacity forecast_disk_growth prefers
// over the declared one while it is fresh (forecast-06). Call it before
// the first Forecast.
func (f *Forecaster) WithCapacity(src runway.CapacitySource) *Forecaster {
	f.capacity = src
	return f
}

// diskCapacity is this run's disk capacity.
func (f *Forecaster) diskCapacity(ctx context.Context) Capacity {
	return ResolveCapacity(ctx, f.cfg.DiskCapacityBytes, f.capacity)
}

// Categories produced from each daily aggregate source.
var (
	systemForecastCategories = []string{
		"forecast_disk_growth", "forecast_connection_saturation",
		"forecast_cache_pressure", "forecast_checkpoint_pressure",
	}
	queryForecastCategories = []string{"forecast_query_volume"}
)

// Forecast runs all forecast rules and returns findings. A data source
// that fails is logged and skipped; LastEvaluatedCategories then omits
// its categories so their open findings are not resolved (G2-B02).
func (f *Forecaster) Forecast(
	ctx context.Context,
) ([]analyzer.Finding, error) {
	var all []analyzer.Finding
	var evaluated []string

	if sysAggs, err := QueryDailySystemAggs(ctx, f.pool, f.cfg.LookbackDays); err != nil {
		f.logFn("WARN", "forecaster: system aggs: %v", err)
	} else {
		gateCacheDays(sysAggs, cacheMinBlocks(f.cfg))
		cache := forecastCachePressure(sysAggs, f.cfg)
		f.addCacheEvidence(ctx, cache)
		all = append(all, withDiskCapacity(forecastDiskGrowth(sysAggs, f.cfg),
			f.diskCapacity(ctx))...)
		all = append(all, forecastConnectionSaturation(sysAggs, f.cfg)...)
		all = append(all, cache...)
		all = append(all, forecastCheckpointPressure(sysAggs, f.cfg)...)
		evaluated = append(evaluated, evaluatedSystemCategories(sysAggs)...)
	}
	if qAggs, err := f.dailyQueryAggs(ctx); err != nil {
		f.logFn("WARN", "forecaster: query aggs: %v", err)
	} else {
		all = append(all, forecastQueryVolume(qAggs, f.cfg)...)
		evaluated = append(evaluated, queryForecastCategories...)
	}

	f.mu.Lock()
	f.lastEvaluated = evaluated
	f.mu.Unlock()
	return all, nil
}

// evaluatedSystemCategories lists the system categories a read of aggs
// evaluates. With minDataPoints days of history but fewer days of
// measured size, the disk trend is unknown rather than flat, so an open
// disk finding must not be resolved on missing data (forecast-07). The
// checkpoint rate needs minDataPoints usable intervals of the split
// counters (forecast-03); legacy rows carry none. The cache ratio needs
// minDataPoints days with interval counters (forecast-04).
func evaluatedSystemCategories(aggs []DaySystemAgg) []string {
	diskUnknown := len(aggs) >= minDataPoints &&
		len(knownSizeDays(aggs)) < minDataPoints
	checkpointUnknown := !checkpointEvaluable(aggs)
	cacheUnknown := !cacheEvaluable(aggs)
	cats := make([]string, 0, len(systemForecastCategories))
	for _, c := range systemForecastCategories {
		if diskUnknown && c == "forecast_disk_growth" ||
			checkpointUnknown && c == "forecast_checkpoint_pressure" ||
			cacheUnknown && c == "forecast_cache_pressure" {
			continue
		}
		cats = append(cats, c)
	}
	return cats
}

// LastEvaluatedCategories reports the forecast categories whose data
// source was read successfully by the most recent Forecast call.
func (f *Forecaster) LastEvaluatedCategories() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastEvaluated...)
}
