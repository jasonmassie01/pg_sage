package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

func snapshotLatestHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" {
			database = "all"
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, database)
		if !ok {
			if database == "all" {
				jsonResponse(w, map[string]any{
					"database": "all", "snapshot": nil,
				})
				return
			}
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		metric := r.URL.Query().Get("metric")
		if metric == "" {
			metric = "system"
		}
		writeLatestSnapshot(w, r, selected, metric)
	}
}

// writeLatestSnapshot answers the newest snapshot of metric; no pool or
// no row is a null snapshot, any other read error a 500.
func writeLatestSnapshot(
	w http.ResponseWriter, r *http.Request, selected namedPool, metric string,
) {
	if selected.pool == nil {
		if selected.name == "" {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]any{
			"database": selected.name, "snapshot": nil,
		})
		return
	}
	displayName := selected.name
	data, err := querySnapshotLatest(
		r.Context(), selected.pool, metric,
	)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			internalError(w, r, "read latest snapshot", err)
			return
		}
		jsonResponse(w, map[string]any{
			"database": displayName, "snapshot": nil,
		})
		return
	}
	jsonResponse(w, map[string]any{
		"database": displayName, "snapshot": data,
	})
}

func snapshotHistoryHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		metric := q.Get("metric")
		if database == "" {
			database = "all"
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, database)
		if !ok {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if msg := historyMetricProblem(metric); msg != "" {
			jsonError(w, msg, http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			if selected.name == "" {
				jsonError(w, "database not found", http.StatusNotFound)
				return
			}
			jsonResponse(w, map[string]any{
				"database": selected.name, "metric": metric,
				"points": []any{}, "truncated": false,
			})
			return
		}
		writeSnapshotHistory(w, r, selected, metric)
	}
}

// writeSnapshotHistory answers metric's points; a failed read answers
// no points, as the chart treats an empty series.
func writeSnapshotHistory(
	w http.ResponseWriter, r *http.Request, selected namedPool, metric string,
) {
	q := r.URL.Query()
	displayName := selected.name
	hours := parseIntDefault(q.Get("hours"), 24)
	from := parseTimeParam(q.Get("from"))
	to := parseTimeParam(q.Get("to"))
	points, truncated, err := querySnapshotHistory(
		r.Context(), selected.pool, metric, hours, from, to,
	)
	if err != nil {
		slog.Warn("snapshot history query failed; answering no points",
			"database", displayName, "metric", metric, "error", err)
		points, truncated = []historyPoint{}, false
	}
	jsonResponse(w, map[string]any{
		"database": displayName, "metric": metric,
		"points": points, "truncated": truncated,
	})
}

// fleetHealthHandler returns a time-series of health scores per
// database from sage.health_history. Accepts `from`/`to` (RFC3339)
// or `hours` (default 24). When `database=` is set, results are
// filtered to that database; otherwise all databases are included.
// Response shape:
//
//	{
//	  "databases": {
//	    "db1": [{"t": "...", "health": 87, "critical": 1, ...}, ...],
//	    ...
//	  }
//	}
func fleetHealthHandler(
	mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		database := q.Get("database")
		if err := validateDatabaseParam(database); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		from, to := fleetHealthWindow(q)
		jsonResponse(w, map[string]any{
			"from":      from,
			"to":        to,
			"databases": fleetHealthSeries(r.Context(), mgr, database, from, to),
		})
	}
}

// fleetHealthWindow reads from/to, defaulting to the last `hours`
// (24) hours, or an open edge to the epoch or now.
func fleetHealthWindow(q url.Values) (time.Time, time.Time) {
	hours := parseIntDefault(q.Get("hours"), 24)
	from := parseTimeParam(q.Get("from"))
	to := parseTimeParam(q.Get("to"))
	if from.IsZero() && to.IsZero() {
		to = time.Now().UTC()
		from = to.Add(-time.Duration(hours) * time.Hour)
	} else {
		if from.IsZero() {
			from = time.Unix(0, 0)
		}
		if to.IsZero() {
			to = time.Now().UTC()
		}
	}
	return from, to
}

// fleetHealthSeries reads each selected database's health_history; a
// failed read is an empty series.
func fleetHealthSeries(
	ctx context.Context, mgr *fleet.DatabaseManager,
	database string, from, to time.Time,
) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	// In fleet mode every instance has its own sage.health_history
	// written by RecordHealthSnapshots. We query each pool once.
	instances := mgr.Instances()
	for name, inst := range instances {
		if database != "" && database != "all" && database != name {
			continue
		}
		if inst.Pool == nil {
			continue
		}
		points, err := queryHealthHistory(
			ctx, inst.Pool, name, from, to,
		)
		if err != nil {
			slog.Warn("fleet_health: query failed",
				"database", name, "error", err)
			out[name] = []map[string]any{}
			continue
		}
		out[name] = points
	}
	return out
}

func queryHealthHistory(
	ctx context.Context, pool *pgxpool.Pool,
	dbName string, from, to time.Time,
) ([]map[string]any, error) {
	rows, err := pool.Query(ctx,
		`/* pg_sage */ SELECT recorded_at, health_score, findings_open,
		        findings_critical, findings_warning,
		        findings_info, actions_total
		 FROM sage.health_history
		 WHERE database_name = $1
		 AND recorded_at BETWEEN $2 AND $3
		 ORDER BY recorded_at`,
		dbName, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("query health_history: %w", err)
	}
	defer rows.Close()
	points := []map[string]any{}
	for rows.Next() {
		var (
			ts                                            time.Time
			score, open, critical, warning, info, actions int
		)
		if err := rows.Scan(
			&ts, &score, &open, &critical,
			&warning, &info, &actions,
		); err != nil {
			return nil, fmt.Errorf("scan health_history: %w", err)
		}
		points = append(points, map[string]any{
			"t":        ts,
			"health":   score,
			"open":     open,
			"critical": critical,
			"warning":  warning,
			"info":     info,
			"actions":  actions,
		})
	}
	return points, nil
}

func validateMetric(metric string) bool {
	if metric == "" {
		return true
	}
	valid := map[string]bool{
		// Collector snapshot categories.
		"tables": true, "indexes": true, "queries": true,
		"sequences": true, "foreign_keys": true, "system": true,
		"io": true, "locks": true, "config_data": true, "replication": true,
		"partitions": true, "prepared_xacts": true,
		"partitioned_indexes": true, "index_bloat": true, "subscriptions": true,
		"workload_sessions": true, "archiver": true,
		// Dashboard time-series metrics.
		"cache_hit_ratio": true, "connections": true,
		"tps": true, "dead_tuples": true,
		"database_size": true, "replication_lag": true,
	}
	return valid[metric]
}

const snapshotHistoryMaxPoints = 500

func querySnapshotLatest(
	ctx context.Context, pool *pgxpool.Pool, metric string,
) (any, error) {
	var data []byte
	err := histstore.Resolve(pool).QueryRow(ctx,
		`/* pg_sage */ SELECT `+snapstore.DataSQL("s")+` FROM sage.snapshots s
		 WHERE {db:s} AND s.category = $1
		 ORDER BY s.collected_at DESC LIMIT 1`,
		metric,
	).Scan(&data)
	if err != nil {
		return nil, err
	}
	return decodeLatestSnapshot(metric, data)
}
