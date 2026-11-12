package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// The workload explorer (market-01): the heaviest statements of a window
// from the query store, one statement's history, plan and links, and the
// wait-class load the collector samples each cycle. Every route is a
// viewer read; none touches the monitored database's statistics beyond
// pg_stat_statements and pg_sage's own tables.

// Bounds of a workload explorer request.
const (
	defaultTopRows    = 20
	maxTopRows        = 100
	maxWorkloadWindow = 7 * 24 * time.Hour
)

var topOrderParams = map[string]bool{querystore.OrderLoad: true, querystore.OrderCalls: true,
	querystore.OrderMean: true, querystore.OrderRows: true}

// parseWindow reads since and until (RFC 3339): until defaults to now and
// since to def before until. The window must be non-empty and at most
// maxWorkloadWindow long.
func parseWindow(q url.Values, now time.Time, def time.Duration) (time.Time, time.Time,
	error) {
	to, err := timeParam(q, "until", now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	from, err := timeParam(q, "since", to.Add(-def))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("until (%s) must be after since (%s)",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	if to.Sub(from) > maxWorkloadWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("the window is %v; at most %v is read",
			to.Sub(from), maxWorkloadWindow)
	}
	return from, to, nil
}

func timeParam(q url.Values, name string, def time.Time) (time.Time, error) {
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not an RFC 3339 time", name, v)
	}
	return t.UTC(), nil
}

// topParams is a validated top-queries request.
type topParams struct {
	from, to time.Time
	order    string
	limit    int
}

func parseTopParams(q url.Values, now time.Time) (topParams, error) {
	from, to, err := parseWindow(q, now, time.Hour)
	if err != nil {
		return topParams{}, err
	}
	p := topParams{from: from, to: to, order: q.Get("order"), limit: defaultTopRows}
	if p.order == "" {
		p.order = querystore.OrderLoad
	}
	if !topOrderParams[p.order] {
		return topParams{}, fmt.Errorf("order %q is not load, calls, mean or rows", p.order)
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxTopRows {
			return topParams{}, fmt.Errorf("limit %q must be a whole number in 1..%d",
				q.Get("limit"), maxTopRows)
		}
		p.limit = n
	}
	return p, nil
}

// topQueryRow is one ranked statement of one database. queryid is a
// string: a 64-bit hash loses precision as a JSON number in browsers.
type topQueryRow struct {
	Database        string            `json:"database"`
	QueryID         string            `json:"queryid"`
	Query           string            `json:"query"`
	TextUnavailable string            `json:"text_unavailable,omitempty"`
	Tags            map[string]string `json:"sqlcommenter,omitempty"`
	Calls           int64             `json:"calls"`
	TotalTimeMs     float64           `json:"total_time_ms"`
	MeanTimeMs      float64           `json:"mean_time_ms"`
	Rows            int64             `json:"rows"`
	Load            float64           `json:"load"`
	Intervals       int               `json:"intervals"`
	EpochBreaks     int               `json:"epoch_breaks"`
}

// topDatabase is what one database's window covered.
type topDatabase struct {
	Database        string `json:"database"`
	Coverage        string `json:"coverage"`
	NotMeasurable   int    `json:"not_measurable"`
	EpochBreaks     int    `json:"epoch_breaks"`
	ExcludedCount   int    `json:"excluded_count"`
	TextUnavailable string `json:"text_unavailable,omitempty"`
}

const evictedText = "statement evicted from pg_stat_statements (or never tracked " +
	"there); its counters are still in the query store"

// textUnavailableReason is why statement texts are missing when err says
// pg_stat_statements cannot be read here, or "" for any other error.
func textUnavailableReason(err error) string {
	if errors.Is(err, agenttools.ErrUnavailable) {
		return err.Error()
	}
	return ""
}

// mergeTopTexts joins ranked stats with their statements: pg_sage's own
// statements are counted and left out, and a missing text is explained.
// With texts unreadable (err) every row keeps its numbers.
func mergeTopTexts(db string, res querystore.TopResult, texts map[int64]agenttools.Statement,
	err error, limit int) ([]topQueryRow, int, string) {
	reason := textUnavailableReason(err)
	rows := []topQueryRow{}
	excluded := 0
	for _, s := range res.Queries {
		row := topQueryRow{Database: db, QueryID: strconv.FormatInt(s.QueryID, 10),
			Calls: s.Calls, TotalTimeMs: s.TotalMs, MeanTimeMs: s.MeanMs(), Rows: s.Rows,
			Load: round6(s.Load(res.Window)), Intervals: s.Intervals,
			EpochBreaks: s.EpochBreaks, TextUnavailable: reason}
		if reason == "" {
			st, ok := texts[s.QueryID]
			switch {
			case ok && st.Excluded:
				excluded++
				continue
			case ok:
				row.Query, row.Tags = st.Query, st.Tags
			default:
				row.TextUnavailable = evictedText
			}
		}
		if len(rows) < limit {
			rows = append(rows, row)
		}
	}
	return rows, excluded, reason
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// readTopForDatabase ranks one database's window and names its rows.
func readTopForDatabase(ctx context.Context, sel namedPool, p topParams) (topDatabase,
	[]topQueryRow, error) {
	res, err := querystore.Top(ctx, sel.pool, querystore.TopRequest{From: p.from, To: p.to,
		Order: p.order, Limit: querystore.MaxTopScan})
	if err != nil {
		return topDatabase{}, nil, err
	}
	ids := make([]int64, len(res.Queries))
	for i, s := range res.Queries {
		ids[i] = s.QueryID
	}
	var texts map[int64]agenttools.Statement
	var textErr error
	if len(ids) > 0 {
		texts, textErr = agenttools.New(sel.pool, agenttools.Options{}).Statements(ctx, ids)
		if textErr != nil && textUnavailableReason(textErr) == "" {
			return topDatabase{}, nil, textErr
		}
	}
	rows, excluded, reason := mergeTopTexts(sel.name, res, texts, textErr, p.limit)
	return topDatabase{Database: sel.name, Coverage: res.Coverage,
		NotMeasurable: res.NotMeasurable, EpochBreaks: res.EpochBreaks,
		ExcludedCount: excluded, TextUnavailable: reason}, rows, nil
}

// fleetCoverage is the coverage of several databases' windows: theirs
// when they agree, partial when they differ, none without a database.
func fleetCoverage(dbs []topDatabase) string {
	if len(dbs) == 0 {
		return querystore.CoverageNone
	}
	for _, d := range dbs[1:] {
		if d.Coverage != dbs[0].Coverage {
			return querystore.CoveragePartial
		}
	}
	return dbs[0].Coverage
}

func queriesTopHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		p, err := parseTopParams(r.URL.Query(), time.Now().UTC())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if database == "" {
			database = "all"
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		dbs, rows := []topDatabase{}, []topQueryRow{}
		reason := ""
		for _, sel := range poolsForDatabaseSelection(mgr, database) {
			d, dbRows, err := readTopForDatabase(r.Context(), sel, p)
			if err != nil {
				internalError(w, r, "read the top queries of "+sel.name, err)
				return
			}
			dbs, rows = append(dbs, d), append(rows, dbRows...)
			if reason == "" {
				reason = d.TextUnavailable
			}
		}
		body := map[string]any{"database": responseDatabaseName(database),
			"since": p.from, "until": p.to, "order": p.order, "limit": p.limit,
			"coverage": fleetCoverage(dbs), "databases": dbs, "queries": rows}
		if reason != "" {
			body["text_unavailable"] = reason
		}
		jsonResponse(w, body)
	}
}
