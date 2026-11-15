package workpack

import (
	"context"
	"strconv"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// connections.json: the workload_sessions snapshots of the window summed
// per hour of week (in the header's timezone) in the database, then made
// a mean per snapshot here. An hour with a snapshot written before ages
// were collected has ages unknown, never zero.

// connHourAgg is one hour-of-week bucket's sums.
type connHourAgg struct {
	snapshots, withAges int
	states              map[string]float64
	ages                map[string]map[string]float64
}

// connectionsSQL returns rows (hour, kind, state, bucket, value): kind n
// is the snapshot count, a the count with ages, s the sessions of a
// state, g the sessions of a state and age bucket.
var connectionsSQL = `/* pg_sage */ WITH w AS (
    SELECT ((extract(isodow FROM s.collected_at AT TIME ZONE $3)::int - 1) * 24
            + extract(hour FROM s.collected_at AT TIME ZONE $3)::int) AS how,
           ` + snapstore.DataSQL("s") + ` AS d
      FROM sage.snapshots s
     WHERE {db:s} AND s.category = 'workload_sessions' AND s.collected_at >= $1
       AND s.collected_at < $2 AND ` + snapstore.NonEmptySQL("s") + `)
SELECT w.how, 'n', '', '', count(*)::float8 FROM w GROUP BY w.how
UNION ALL
SELECT w.how, 'a', '', '', count(*)::float8 FROM w
 WHERE jsonb_typeof(w.d->'ages') = 'array' GROUP BY w.how
UNION ALL
SELECT w.how, 's', COALESCE(g->>'state', 'unknown'), '', sum((g->>'sessions')::float8)
  FROM w CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(w.d->'groups') =
       'array' THEN w.d->'groups' ELSE '[]'::jsonb END) g
 GROUP BY 1, 2, 3
UNION ALL
SELECT w.how, 'g', COALESCE(a->>'state', 'unknown'), COALESCE(a->>'bucket', 'unknown'),
       sum((a->>'sessions')::float8)
  FROM w CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(w.d->'ages') =
       'array' THEN w.d->'ages' ELSE '[]'::jsonb END) a
 GROUP BY 1, 2, 3, 4`

func (r reader) connections(ctx context.Context, h *history, tz string) error {
	rows, err := r.st.Query(ctx, connectionsSQL, r.w.From, r.w.To, tz)
	if err != nil {
		return r.fail("sage.snapshots (workload_sessions)", err)
	}
	aggs := map[int]*connHourAgg{}
	for rows.Next() {
		var how int
		var kind, state, bucket string
		var v float64
		if err := rows.Scan(&how, &kind, &state, &bucket, &v); err != nil {
			rows.Close()
			return r.fail("sage.snapshots (workload_sessions)", err)
		}
		addConnRow(aggs, how, kind, state, bucket, v)
	}
	if err := rows.Err(); err != nil {
		return r.fail("sage.snapshots (workload_sessions)", err)
	}
	h.maint.conn = aggs
	return nil
}

func addConnRow(aggs map[int]*connHourAgg, how int, kind, state, bucket string, v float64) {
	a := aggs[how]
	if a == nil {
		a = &connHourAgg{states: map[string]float64{}, ages: map[string]map[string]float64{}}
		aggs[how] = a
	}
	switch kind {
	case "n":
		a.snapshots = int(v)
	case "a":
		a.withAges = int(v)
	case "s":
		a.states[state] += v
	case "g":
		if a.ages[state] == nil {
			a.ages[state] = map[string]float64{}
		}
		a.ages[state][bucket] += v
	}
}

// buildConnections is the 168 hours, Monday 00:00 first.
func buildConnections(aggs map[int]*connHourAgg) Connections {
	out := Connections{Hours: make([]ConnectionHour, baseline.BucketsPerWeek)}
	for how := range out.Hours {
		hour := ConnectionHour{ID: "connections/hour/" + strconv.Itoa(how), Hour: how,
			States: valueUnknown, Ages: valueUnknown}
		if a := aggs[how]; a != nil && a.snapshots > 0 {
			n := float64(a.snapshots)
			hour.Snapshots = a.snapshots
			states := map[string]float64{}
			for st, v := range a.states {
				states[st] = round12(v / n)
			}
			hour.States = states
			if a.withAges == a.snapshots {
				ages := map[string]map[string]float64{}
				for st, buckets := range a.ages {
					ages[st] = map[string]float64{}
					for b, v := range buckets {
						ages[st][b] = round12(v / n)
					}
				}
				hour.Ages = ages
			}
		}
		out.Hours[how] = hour
	}
	return out
}
