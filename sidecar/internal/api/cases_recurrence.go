package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
)

// recurrenceMaxDepth bounds the walk back along previous_incident_id.
const recurrenceMaxDepth = 1000

// recurrenceSQL walks each recurring incident's chain of predecessors in
// one statement: the row count is the occurrences (this incident
// included) and depth 2 is the direct predecessor. The path stops a
// hand-made cycle; recurrenceMaxDepth bounds the walk.
const recurrenceSQL = `/* pg_sage */WITH RECURSIVE chain AS (
  SELECT i.id AS start_id, i.previous_incident_id AS prev,
         NULL::timestamptz AS resolved_at, 1 AS depth, ARRAY[i.id] AS path
    FROM sage.incidents i
   WHERE i.id = ANY($1::text[]::uuid[])
  UNION ALL
  SELECT c.start_id, p.previous_incident_id, p.resolved_at, c.depth + 1, c.path || p.id
    FROM chain c
    JOIN sage.incidents p ON p.id = c.prev
   WHERE NOT p.id = ANY(c.path) AND c.depth < $2
)
SELECT start_id::text, count(*)::int,
       max(resolved_at) FILTER (WHERE depth = 2)
  FROM chain
 GROUP BY start_id`

type recurrenceRow struct {
	count    int
	resolved *time.Time
}

// enrichIncidentRecurrence fills the count and the predecessor's
// resolution of every recurring incident case in list with one batched
// lookup (cases-07). A failed lookup keeps each link and marks its
// history unavailable rather than reporting a first occurrence.
func enrichIncidentRecurrence(ctx context.Context, pool *pgxpool.Pool, list []cases.Case) {
	byID := map[string][]int{}
	ids := []string{}
	for i, c := range list {
		if c.Recurrence == nil || len(c.SourceIDs) == 0 {
			continue
		}
		id := c.SourceIDs[0]
		if _, seen := byID[id]; !seen {
			ids = append(ids, id)
		}
		byID[id] = append(byID[id], i)
	}
	if len(ids) == 0 {
		return
	}
	found, err := readRecurrence(ctx, pool, ids)
	if err != nil {
		slog.Warn("cases: incident recurrence lookup failed; history marked unavailable",
			"incidents", len(ids), "error", err)
	}
	for id, idx := range byID {
		row, ok := found[id]
		for _, i := range idx {
			r := list[i].Recurrence
			r.HistoryUnavailable = !ok
			if ok {
				r.Count, r.PreviousResolvedAt = row.count, row.resolved
			}
		}
	}
}

func readRecurrence(
	ctx context.Context, pool *pgxpool.Pool, ids []string,
) (map[string]recurrenceRow, error) {
	if pool == nil {
		return nil, fmt.Errorf("read incident recurrence: no database pool")
	}
	rows, err := pool.Query(ctx, recurrenceSQL, ids, recurrenceMaxDepth)
	if err != nil {
		return nil, fmt.Errorf("read incident recurrence for %d incidents: %w", len(ids), err)
	}
	defer rows.Close()
	out := make(map[string]recurrenceRow, len(ids))
	for rows.Next() {
		var id string
		var row recurrenceRow
		if err := rows.Scan(&id, &row.count, &row.resolved); err != nil {
			return nil, fmt.Errorf("scan incident recurrence: %w", err)
		}
		if row.count > 1 {
			out[id] = row
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate incident recurrence: %w", err)
	}
	return out, nil
}
