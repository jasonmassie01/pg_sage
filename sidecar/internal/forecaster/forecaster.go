package forecaster

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/histstore"
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
	// MinRSquared is the fit a disk growth or connection saturation trend
	// needs before it is projected (forecast-08); 0 means 0.5.
	MinRSquared float64
	// CollectorInterval is collector.interval_seconds: a day counts when
	// its snapshots cover 80% of its hours (forecast-08). 0 measures no
	// coverage (callers without a collector).
	CollectorInterval time.Duration
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
	// owner elects the fleet database that raises the cluster-wide
	// forecasts (nil: this one does); set before the analyzer starts.
	owner ClusterOwner

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

// Forecast runs all forecast rules on closed calendar days and returns
// findings (forecast-08: today's partial day is left out). A data source
// that fails is logged and skipped; LastEvaluatedCategories then omits
// its categories so their open findings are not resolved (G2-B02).
func (f *Forecaster) Forecast(
	ctx context.Context,
) ([]analyzer.Finding, error) {
	var all []analyzer.Finding
	var evaluated []string
	// Today is read before the history: a day that ends between the two
	// reads is then left out once more, never read as closed while open.
	today, err := sessionToday(ctx, f.pool)
	if err != nil {
		f.logFn("WARN", "forecaster: %v", err)
	} else {
		all, evaluated = f.systemForecast(ctx, today)
		q, qEvaluated := f.queryForecast(ctx, today)
		all, evaluated = append(all, q...), append(evaluated, qEvaluated...)
	}
	all, evaluated = f.scopeCluster(all, evaluated)
	f.mu.Lock()
	f.lastEvaluated = evaluated
	f.mu.Unlock()
	return all, nil
}

// sessionTodaySQL is the start of the history store session's current day.
const sessionTodaySQL = `/* pg_sage */ SELECT date_trunc('day', now())`

func sessionToday(ctx context.Context, pool *pgxpool.Pool) (time.Time, error) {
	var today time.Time
	if err := histstore.Resolve(pool).QueryRow(ctx, sessionTodaySQL).Scan(&today); err != nil {
		return time.Time{}, fmt.Errorf("read the session's current day: %w", err)
	}
	return today, nil
}

// systemForecast runs the four system rules on the closed days.
func (f *Forecaster) systemForecast(ctx context.Context,
	today time.Time) ([]analyzer.Finding, []string) {
	raw, err := QueryDailySystemAggs(ctx, f.pool, f.cfg.LookbackDays)
	if err != nil {
		f.logFn("WARN", "forecaster: system aggs: %v", err)
		return nil, nil
	}
	r := systemFindings(closedSystemDays(raw, today, f.cfg.CollectorInterval), f.cfg)
	f.addCacheEvidence(ctx, r.cache)
	all := withDiskCapacity(r.disk, f.diskCapacity(ctx))
	all = append(all, r.connection...)
	all = append(all, r.cache...)
	all = append(all, r.checkpoint...)
	return all, r.evaluated
}

// queryForecast runs the query volume rule on the closed days; the
// category is evaluated only with two complete weeks (forecast-08).
func (f *Forecaster) queryForecast(ctx context.Context,
	today time.Time) ([]analyzer.Finding, []string) {
	raw, err := f.dailyQueryAggs(ctx)
	if err != nil {
		f.logFn("WARN", "forecaster: query aggs: %v", err)
		return nil, nil
	}
	days := closedQueryDays(raw, today, f.cfg.CollectorInterval)
	if _, ok := weekOverWeek(days); !ok {
		return nil, nil
	}
	return forecastQueryVolume(days, f.cfg), queryForecastCategories
}

// systemResult is what the system rules found on closed days.
type systemResult struct {
	disk, connection, cache, checkpoint []analyzer.Finding
	evaluated                           []string
}

// systemFindings runs the system rules on closed days (closedSystemDays).
// The disk capacity and the cache evidence need reads: Forecast adds them.
func systemFindings(days []DaySystemAgg, cfg ForecasterConfig) systemResult {
	days = append([]DaySystemAgg(nil), days...)
	gateCacheDays(days, cacheMinBlocks(cfg))
	return systemResult{
		disk:       forecastDiskGrowth(days, cfg),
		connection: forecastConnectionSaturation(days, cfg),
		cache:      forecastCachePressure(days, cfg),
		checkpoint: forecastCheckpointPressure(days, cfg),
		evaluated:  evaluatedSystemCategories(days),
	}
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
