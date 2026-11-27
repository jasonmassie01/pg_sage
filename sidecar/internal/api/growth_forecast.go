package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// ---------- Growth Forecasts ----------

// growthForecastHandler serves GET /api/v1/forecasts/growth: per database,
// the growth of the cluster's database bytes the runway monitor samples
// (forecast-06), with the disk capacity and time to full.
func growthForecastHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		days := min(parseIntDefault(q.Get("days"), 90), 365)
		forecasts := []map[string]any{}
		for _, selected := range poolsForDatabaseSelection(mgr, database) {
			forecast, err := queryGrowthForecast(r.Context(), selected.pool, days)
			if err != nil {
				slog.Error("query growth forecasts failed",
					"database", selected.name, "error", err)
				jsonError(w, "failed to query growth forecasts", 500)
				return
			}
			if forecast == nil {
				continue
			}
			forecast["database_name"] = selected.name
			forecast["fleet_database_name"] = selected.name
			addGrowthCapacity(r.Context(), mgr, selected.name, forecast)
			forecasts = append(forecasts, forecast)
		}
		jsonResponse(w, map[string]any{
			"database":  responseDatabaseName(database),
			"forecasts": forecasts,
		})
	}
}

// addGrowthCapacity adds the database's disk capacity fields, as
// forecast_disk_growth carries them (undeclared without a resolver).
func addGrowthCapacity(ctx context.Context, mgr *fleet.DatabaseManager, name string,
	forecast map[string]any) {
	c := forecaster.Capacity{Source: forecaster.CapacityUndeclared}
	if inst := mgr.GetInstance(name); inst != nil && inst.DiskCapacity != nil {
		c = inst.DiskCapacity(ctx)
	}
	latest, _ := forecast["latest_bytes"].(float64)
	slope, _ := forecast["growth_rate_bytes_day"].(float64)
	c.AddTo(forecast, latest, slope)
}

// growthForecastSQL fits the newest epoch of the database_bytes series
// over the window: a trend never spans two epochs. It needs two samples.
const growthForecastSQL = `/* pg_sage */WITH newest AS (
    SELECT epoch FROM sage.runway_samples
     WHERE kind = $1 AND subject = $2
     ORDER BY sampled_at DESC LIMIT 1)
SELECT count(*)::int,
       regr_slope(s.value, extract(epoch FROM s.sampled_at)::float8) * 86400,
       regr_r2(s.value, extract(epoch FROM s.sampled_at)::float8),
       (array_agg(s.value ORDER BY s.sampled_at))[1],
       (array_agg(s.value ORDER BY s.sampled_at DESC))[1],
       min(s.sampled_at), max(s.sampled_at)
  FROM sage.runway_samples s JOIN newest n ON n.epoch = s.epoch
 WHERE s.kind = $1 AND s.subject = $2
   AND s.sampled_at > now() - make_interval(days => $3)
HAVING count(*) >= 2`

// queryGrowthForecast is the database's growth forecast, nil without two
// samples in the window.
func queryGrowthForecast(
	ctx context.Context, pool *pgxpool.Pool, days int,
) (map[string]any, error) {
	var (
		points           int
		slope, r2        *float64
		earliest, latest float64
		first, last      time.Time
	)
	err := pool.QueryRow(ctx, growthForecastSQL, probes.RunwayDatabaseBytes,
		probes.SubjectCluster, days).Scan(&points, &slope, &r2, &earliest, &latest,
		&first, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query growth forecast from sage.runway_samples: %w", err)
	}
	forecast := map[string]any{
		"metric_type": probes.RunwayDatabaseBytes, "object_name": probes.SubjectCluster,
		"earliest_bytes": earliest, "latest_bytes": latest,
		"growth_bytes": latest - earliest, "data_points": points,
		"first_seen": first, "last_seen": last,
	}
	if slope != nil {
		forecast["growth_rate_bytes_day"] = *slope
	}
	if r2 != nil {
		forecast["r2"] = *r2
	}
	return forecast, nil
}
