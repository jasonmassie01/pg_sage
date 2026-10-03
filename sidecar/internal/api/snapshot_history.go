package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// snapshotHistoryMaxBytes caps one history response's documents. Points
// past it are left out and the response says truncated.
const snapshotHistoryMaxBytes = 4 << 20

// perObjectCategories hold one element per table, index, sequence,
// statement, lock or reloption: a single document is megabytes on a
// large database (lifeos: 1.66 MB per indexes row, 666 MB for 24 h of
// history). History serves only bounded categories; the latest document
// stays available from /snapshots/latest.
var perObjectCategories = map[string]bool{
	"tables": true, "indexes": true, "queries": true, "sequences": true,
	"foreign_keys": true, "locks": true, "partitions": true, "config_data": true,
}

// historyMetricProblem says why metric cannot be served as history, or "".
func historyMetricProblem(metric string) string {
	switch {
	case !validateMetric(metric):
		return "invalid metric"
	case perObjectCategories[metric]:
		return "history is not available for the per-object category " + metric +
			"; read the latest document from /api/v1/snapshots/latest"
	}
	return ""
}

// historyPoint is one snapshot document, passed through as stored JSON
// (no decode and re-encode of every document).
type historyPoint struct {
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// querySnapshotHistory returns the category's documents oldest first, at
// most snapshotHistoryMaxPoints of the newest in the window and at most
// snapshotHistoryMaxBytes of documents; truncated reports a cut.
func querySnapshotHistory(
	ctx context.Context, pool *pgxpool.Pool,
	metric string, hours int, from, to time.Time,
) ([]historyPoint, bool, error) {
	rows, err := snapshotHistoryRows(ctx, pool, metric, hours, from, to)
	if err != nil {
		return nil, false, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	points := []historyPoint{}
	size := 0
	for rows.Next() {
		var p historyPoint
		var data []byte
		if err := rows.Scan(&p.Timestamp, &data); err != nil {
			return nil, false, fmt.Errorf("scan snapshot: %w", err)
		}
		if !json.Valid(data) {
			data = []byte("null")
		}
		size += len(data)
		if size > snapshotHistoryMaxBytes {
			return points, true, nil
		}
		p.Data = data
		points = append(points, p)
	}
	return points, false, rows.Err()
}

func snapshotHistoryRows(
	ctx context.Context, pool *pgxpool.Pool,
	metric string, hours int, from, to time.Time,
) (pgx.Rows, error) {
	// Explicit from/to use BETWEEN; otherwise the last-N-hours window.
	if !from.IsZero() || !to.IsZero() {
		if from.IsZero() {
			from = time.Unix(0, 0)
		}
		if to.IsZero() {
			to = time.Now().UTC()
		}
		return pool.Query(ctx, `/* pg_sage */ SELECT collected_at, `+snapstore.DataSQL("")+`
			 FROM (SELECT collected_at, data, base_id FROM sage.snapshots
			       WHERE category = $1 AND collected_at BETWEEN $2 AND $3
			       ORDER BY collected_at DESC LIMIT $4) capped
			 ORDER BY collected_at`,
			metric, from, to, snapshotHistoryMaxPoints)
	}
	return pool.Query(ctx, `/* pg_sage */ SELECT collected_at, `+snapstore.DataSQL("")+`
		 FROM (SELECT collected_at, data, base_id FROM sage.snapshots
		       WHERE category = $1
		         AND collected_at > now() - ($2 || ' hours')::interval
		       ORDER BY collected_at DESC LIMIT $3) capped
		 ORDER BY collected_at`,
		metric, strconv.Itoa(hours), snapshotHistoryMaxPoints)
}
