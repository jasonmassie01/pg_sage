package forecaster

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

// ForecasterConfig holds thresholds for workload forecasting.
type ForecasterConfig struct {
	Enabled              bool
	LookbackDays         int
	DiskWarnGrowthGBDay  float64
	ConnectionWarnPct    float64
	CacheWarnThreshold   float64
	SequenceWarnDays     int
	SequenceCriticalDays int
	// v0.9: storage growth forecasting fields.
	MinDataPoints     int
	AlertHorizons     []int
	DiskCapacityBytes int64
	MinRSquared       float64
}

// Forecaster produces capacity forecast findings from historical
// snapshot data using statistical methods.
type Forecaster struct {
	pool  *pgxpool.Pool
	cfg   ForecasterConfig
	logFn func(string, string, ...any)

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

// Categories produced from each daily aggregate source.
var (
	systemForecastCategories = []string{
		"forecast_disk_growth", "forecast_connection_saturation",
		"forecast_cache_pressure", "forecast_checkpoint_pressure",
	}
	queryForecastCategories    = []string{"forecast_query_volume"}
	sequenceForecastCategories = []string{"forecast_sequence_exhaustion"}
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
		all = append(all, forecastDiskGrowth(sysAggs, f.cfg)...)
		all = append(all, forecastConnectionSaturation(sysAggs, f.cfg)...)
		all = append(all, forecastCachePressure(sysAggs, f.cfg)...)
		all = append(all, forecastCheckpointPressure(sysAggs, f.cfg)...)
		evaluated = append(evaluated, systemForecastCategories...)
	}
	if qAggs, err := f.dailyQueryAggs(ctx); err != nil {
		f.logFn("WARN", "forecaster: query aggs: %v", err)
	} else {
		all = append(all, forecastQueryVolume(qAggs, f.cfg)...)
		evaluated = append(evaluated, queryForecastCategories...)
	}
	if seqAggs, err := f.dailySeqAggs(ctx); err != nil {
		f.logFn("WARN", "forecaster: seq aggs: %v", err)
	} else {
		all = append(all, forecastSequenceExhaustion(seqAggs, f.cfg)...)
		evaluated = append(evaluated, sequenceForecastCategories...)
	}

	f.mu.Lock()
	f.lastEvaluated = evaluated
	f.mu.Unlock()
	return all, nil
}

// LastEvaluatedCategories reports the forecast categories whose data
// source was read successfully by the most recent Forecast call.
func (f *Forecaster) LastEvaluatedCategories() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastEvaluated...)
}

// ForecastGrowth runs v0.9 storage growth forecasting.
func (f *Forecaster) ForecastGrowth(
	ctx context.Context,
	dbSizeBytes int64,
	dbName string,
) ([]analyzer.Finding, error) {
	if f.cfg.MinDataPoints <= 0 {
		return nil, nil // v0.9 fields not configured
	}

	// Record current size.
	if err := RecordSizeHistory(
		ctx, f.pool, "database", dbName, dbSizeBytes, nil, dbName,
	); err != nil {
		f.logFn("WARN", "forecaster: record size: %v", err)
	}

	// Query history and forecast.
	points, err := QuerySizeHistory(
		ctx, f.pool, "database", dbName, f.cfg.LookbackDays,
	)
	if err != nil {
		return nil, fmt.Errorf("query size history: %w", err)
	}

	forecast := forecastGrowth(
		points, f.cfg.DiskCapacityBytes,
		f.cfg.MinDataPoints, f.cfg.MinRSquared,
	)
	if forecast == nil {
		return nil, nil
	}
	forecast.DatabaseName = dbName

	return GrowthFindings(
		[]LinearForecast{*forecast}, f.cfg.MinRSquared,
	), nil
}
