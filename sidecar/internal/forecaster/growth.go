package forecaster

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
)

// LinearForecast represents a storage growth prediction.
type LinearForecast struct {
	Metric          string    `json:"metric"`
	ObjectName      string    `json:"object_name"`
	CurrentBytes    int64     `json:"current_bytes"`
	GrowthRateBytes int64     `json:"growth_rate_bytes_per_day"`
	ThresholdBytes  int64     `json:"threshold_bytes"`
	DaysUntilFull   int       `json:"days_until_full"`
	ProjectedDate   time.Time `json:"projected_date"`
	R2              float64   `json:"r2"`
	DataPoints      int       `json:"data_points"`
	DatabaseName    string    `json:"database_name,omitempty"`
}

// SizeDataPoint is a single historical size measurement.
type SizeDataPoint struct {
	CollectedAt time.Time
	SizeBytes   int64
}

const dbTimeout = 10 * time.Second

const secondsPerDay = 86400

// linearRegression implements ordinary least squares on
// (unix_timestamp, size_bytes) pairs. Returns slope in bytes/sec,
// y-intercept, and coefficient of determination (R2).
func linearRegression(
	points []SizeDataPoint,
) (slope, intercept, r2 float64) {
	n := float64(len(points))
	if n < 2 {
		return 0, 0, 0
	}

	var sumX, sumY, sumXY, sumX2 float64
	for _, p := range points {
		x := float64(p.CollectedAt.Unix())
		y := float64(p.SizeBytes)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}

	denom := n*sumX2 - sumX*sumX
	if denom == 0 {
		return 0, 0, 0
	}

	slope = (n*sumXY - sumX*sumY) / denom
	intercept = (sumY - slope*sumX) / n

	meanY := sumY / n
	var ssTot, ssRes float64
	for _, p := range points {
		x := float64(p.CollectedAt.Unix())
		y := float64(p.SizeBytes)
		diff := y - meanY
		ssTot += diff * diff
		residual := y - (slope*x + intercept)
		ssRes += residual * residual
	}

	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	return slope, intercept, r2
}

// forecastGrowth builds a LinearForecast from historical size data.
// Returns nil when there are too few data points.
func forecastGrowth(
	points []SizeDataPoint,
	capacityBytes int64,
	minDataPts int,
	minR2 float64,
) *LinearForecast {
	if len(points) < minDataPts {
		return nil
	}

	slope, _, r2 := linearRegression(points)

	latest := points[len(points)-1]
	growthPerDay := slope * secondsPerDay

	fc := &LinearForecast{
		CurrentBytes:    latest.SizeBytes,
		GrowthRateBytes: int64(math.Round(growthPerDay)),
		ThresholdBytes:  capacityBytes,
		R2:              r2,
		DataPoints:      len(points),
		DaysUntilFull:   -1,
	}

	if slope <= 0 || capacityBytes <= 0 {
		return fc
	}

	remaining := float64(capacityBytes) - float64(latest.SizeBytes)
	if remaining <= 0 {
		fc.DaysUntilFull = 0
		fc.ProjectedDate = latest.CollectedAt
		return fc
	}

	days := remaining / growthPerDay
	fc.DaysUntilFull = int(math.Ceil(days))
	fc.ProjectedDate = latest.CollectedAt.Add(
		time.Duration(days*24) * time.Hour,
	)
	return fc
}

// --- Database operations ---

const recordSizeSQL = `
INSERT INTO sage.size_history
    (metric_type, object_name, size_bytes, dead_tuple_pct,
     database_name, collected_at)
VALUES ($1, $2, $3, $4, $5, now())`

// RecordSizeHistory persists a size measurement for later
// forecasting. Called each collector cycle.
func RecordSizeHistory(
	ctx context.Context,
	pool *pgxpool.Pool,
	metricType, objectName string,
	sizeBytes int64,
	deadTuplePct *float64,
	dbName string,
) error {
	qCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	_, err := pool.Exec(qCtx, recordSizeSQL,
		metricType, objectName, sizeBytes,
		deadTuplePct, dbName,
	)
	if err != nil {
		return fmt.Errorf(
			"record size history (%s/%s): %w",
			metricType, objectName, err,
		)
	}
	return nil
}

const querySizeSQL = `
SELECT collected_at, size_bytes
FROM sage.size_history
WHERE metric_type = $1
  AND object_name = $2
  AND collected_at > now() - make_interval(days => $3)
ORDER BY collected_at`

// QuerySizeHistory reads historical size data for a given
// metric and object over the lookback window.
func QuerySizeHistory(
	ctx context.Context,
	pool *pgxpool.Pool,
	metricType, objectName string,
	lookbackDays int,
) ([]SizeDataPoint, error) {
	qCtx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	rows, err := pool.Query(
		qCtx, querySizeSQL,
		metricType, objectName, lookbackDays,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"query size history (%s/%s): %w",
			metricType, objectName, err,
		)
	}
	defer rows.Close()

	var points []SizeDataPoint
	for rows.Next() {
		var p SizeDataPoint
		if err := rows.Scan(
			&p.CollectedAt, &p.SizeBytes,
		); err != nil {
			return nil, fmt.Errorf(
				"scan size history: %w", err,
			)
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterate size history: %w", err,
		)
	}
	return points, nil
}

// --- Finding generation ---

// GrowthFindings converts storage forecasts into analyzer findings.
// Only forecasts within actionable horizons produce findings.
func GrowthFindings(
	forecasts []LinearForecast,
	minR2 float64,
) []analyzer.Finding {
	var findings []analyzer.Finding
	for _, fc := range forecasts {
		f := growthFinding(fc, minR2)
		if f != nil {
			findings = append(findings, *f)
		}
	}
	return findings
}

func growthFinding(
	fc LinearForecast,
	minR2 float64,
) *analyzer.Finding {
	days := fc.DaysUntilFull
	r2 := fc.R2

	// No finding for shrinking/stable or distant projections.
	if days < 0 || days > 30 {
		return nil
	}

	severity, reliable := growthSeverity(days, r2, minR2)
	if severity == "" {
		return nil
	}

	title := fmt.Sprintf(
		"%s %s projected full in %d days",
		fc.Metric, fc.ObjectName, days,
	)
	rec := fmt.Sprintf(
		"Storage for %s is projected to reach capacity "+
			"in %d days at current growth rate of %d bytes/day. "+
			"Review retention policies and consider expanding "+
			"storage.",
		fc.ObjectName, days, fc.GrowthRateBytes,
	)

	if !reliable {
		title += " (forecast unreliable)"
		rec = "Insufficient statistical confidence (R2=" +
			fmt.Sprintf("%.2f", r2) +
			") for a reliable projection. Collect more data."
	}

	return &analyzer.Finding{
		Category:         "storage_forecast",
		Severity:         severity,
		ObjectType:       fc.Metric,
		ObjectIdentifier: fc.ObjectName,
		Title:            title,
		Detail: map[string]any{
			"forecast_type":         "storage_growth",
			"current_bytes":         fc.CurrentBytes,
			"growth_rate_bytes_day": fc.GrowthRateBytes,
			"threshold_bytes":       fc.ThresholdBytes,
			"days_until_full":       fc.DaysUntilFull,
			"projected_date":        fc.ProjectedDate,
			"r_squared":             r2,
			"data_points":           fc.DataPoints,
		},
		DatabaseName:   fc.DatabaseName,
		Recommendation: rec,
	}
}

// growthSeverity returns the severity and whether the forecast is
// considered statistically reliable. Empty severity means no finding.
func growthSeverity(
	days int, r2, minR2 float64,
) (severity string, reliable bool) {
	switch {
	case days <= 3 && r2 >= 0.8:
		return "critical", true
	case days <= 7 && r2 >= 0.5:
		return "warning", true
	case days <= 30 && r2 >= 0.5:
		return "info", true
	case r2 < minR2 && days <= 30:
		return "info", false
	default:
		return "", false
	}
}
