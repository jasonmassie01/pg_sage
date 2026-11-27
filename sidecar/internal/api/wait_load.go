package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// The workload explorer's wait-load chart (market-01): the collector's
// wait_events snapshot category, one count of active client sessions by
// wait class per cycle. It is sampled each collector cycle, not active
// session history. Where the provider reports load by wait class
// (Performance Insights, or Lakebase wait counters in market-01b's
// wait_load_lakebase.go), that breakdown is preferred and named.

// Sources of a wait-load answer.
const (
	waitSourceActivity = "pg_stat_activity"
	waitSourcePI       = "performance_insights"
	waitSampling       = "sampled each collector cycle (not active session history)"
)

// maxWaitSamples bounds one wait-load read (the newest are kept).
const maxWaitSamples = 2000

// maxWaitEvents bounds the events table of a wait-load answer.
const maxWaitEvents = 20

type waitRow struct {
	WaitEventType string `json:"wait_event_type"`
	WaitEvent     string `json:"wait_event"`
	Sessions      int    `json:"sessions"`
}

type waitPoint struct {
	T        time.Time      `json:"t"`
	Sessions int            `json:"sessions"`
	ByClass  map[string]int `json:"by_class"`
}

var waitSamplesSQL = `/* pg_sage */ SELECT collected_at, ` + snapstore.DataSQL("") + `
	FROM (SELECT s.collected_at, s.data, s.base_id FROM sage.snapshots s
	      WHERE {db:s} AND s.category = 'wait_events' AND s.collected_at BETWEEN $1 AND $2
	      ORDER BY s.collected_at DESC LIMIT $3) capped
	ORDER BY collected_at`

// readWaitSamples reads the window's samples, oldest first; truncated
// reports that older samples were left out.
func readWaitSamples(ctx context.Context, pool *pgxpool.Pool, from, to time.Time,
) ([]time.Time, [][]waitRow, bool, error) {
	rows, err := histstore.Resolve(pool).Query(ctx, waitSamplesSQL, from, to, maxWaitSamples)
	if err != nil {
		return nil, nil, false, fmt.Errorf("read wait samples: %w", err)
	}
	defer rows.Close()
	var times []time.Time
	var docs [][]waitRow
	for rows.Next() {
		var at time.Time
		var data []byte
		if err := rows.Scan(&at, &data); err != nil {
			return nil, nil, false, fmt.Errorf("scan wait sample: %w", err)
		}
		var doc []waitRow
		if data == nil || json.Unmarshal(data, &doc) != nil {
			continue // a delta whose keyframe is gone: unknown, not zero
		}
		times, docs = append(times, at), append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, fmt.Errorf("read wait samples: %w", err)
	}
	return times, docs, len(times) >= maxWaitSamples, nil
}

// waitSeries builds the chart series, the classes by total sessions
// (largest first) and the heaviest events.
func waitSeries(times []time.Time, docs [][]waitRow) ([]waitPoint, []string, []waitRow) {
	series := make([]waitPoint, len(times))
	classTotal := map[string]int{}
	events := map[[2]string]int{}
	for i, doc := range docs {
		p := waitPoint{T: times[i], ByClass: map[string]int{}}
		for _, r := range doc {
			p.ByClass[r.WaitEventType] += r.Sessions
			p.Sessions += r.Sessions
			classTotal[r.WaitEventType] += r.Sessions
			events[[2]string{r.WaitEventType, r.WaitEvent}] += r.Sessions
		}
		series[i] = p
	}
	classes := make([]string, 0, len(classTotal))
	for c := range classTotal {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool {
		if classTotal[classes[i]] != classTotal[classes[j]] {
			return classTotal[classes[i]] > classTotal[classes[j]]
		}
		return classes[i] < classes[j]
	})
	top := make([]waitRow, 0, len(events))
	for k, n := range events {
		top = append(top, waitRow{WaitEventType: k[0], WaitEvent: k[1], Sessions: n})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].Sessions != top[j].Sessions {
			return top[i].Sessions > top[j].Sessions
		}
		return top[i].WaitEventType+top[i].WaitEvent < top[j].WaitEventType+top[j].WaitEvent
	})
	if len(top) > maxWaitEvents {
		top = top[:maxWaitEvents]
	}
	return series, classes, top
}

// providerWaitLoad is the provider's current load by wait class for
// database (Performance Insights), or nil when none is reported.
func providerWaitLoad(database string) map[string]any {
	for _, st := range cloudtel.Statuses() {
		if st.Database != database && !slices.Contains(st.Databases, database) {
			continue
		}
		if st.Sample == nil || len(st.Sample.DBLoadByWait) == 0 {
			continue
		}
		return map[string]any{"source": waitSourcePI, "provider": st.Provider,
			"collected_at": st.Sample.CollectedAt, "load_by_wait": st.Sample.DBLoadByWait}
	}
	return nil
}

func waitLoadHandler(mgr *fleet.DatabaseManager, seam LakebaseWaitLoadFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" || database == "all" {
			jsonError(w, "database is required: wait load is per database",
				http.StatusBadRequest)
			return
		}
		from, to, err := parseWindow(r.URL.Query(), time.Now().UTC(), time.Hour)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		body := map[string]any{"database": database, "since": from, "until": to,
			"source": waitSourceActivity, "preferred_source": waitSourceActivity,
			"sampling": waitSampling, "coverage": "none", "truncated": false,
			"series": []waitPoint{}, "classes": []string{}, "events": []waitRow{}}
		if p, src := preferredWaitProvider(r.Context(), seam, database, from, to); p != nil {
			body["provider"], body["preferred_source"] = p, src
		}
		sel, _ := resolveSingleDatabaseRequestPool(mgr, database)
		if sel.pool == nil {
			jsonResponse(w, body)
			return
		}
		times, docs, truncated, err := readWaitSamples(r.Context(), sel.pool, from, to)
		if err != nil {
			internalError(w, r, "read the wait load of "+database, err)
			return
		}
		if len(times) > 0 {
			body["coverage"] = "full"
			body["series"], body["classes"], body["events"] = waitSeries(times, docs)
		}
		body["truncated"] = truncated
		jsonResponse(w, body)
	}
}
