package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/querystore"
)

// The workload explorer's query page (market-01): one statement's
// history, plan changes, latest captured plan, and the cases, hints and
// actions about it. A queryid is per database, so the route needs one.

// queryLinkLimit bounds each list of links.
const queryLinkLimit = 20

// historyPoints is about how many buckets a history shows.
const historyPoints = 120

type historyBucketJSON struct {
	Start       time.Time `json:"start"`
	Calls       int64     `json:"calls"`
	TotalTimeMs float64   `json:"total_time_ms"`
	MeanTimeMs  float64   `json:"mean_time_ms"`
	Rows        int64     `json:"rows"`
	EpochBreak  bool      `json:"epoch_break"`
}

type planChangeJSON struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

type queryPlanJSON struct {
	Source     string          `json:"source"`
	CapturedAt time.Time       `json:"captured_at"`
	TotalCost  float64         `json:"total_cost"`
	PlanHash   string          `json:"plan_hash,omitempty"`
	NodeTypes  []string        `json:"node_types"`
	PlanJSON   json.RawMessage `json:"plan_json,omitempty"`
}

// historyBucket is the bucket that shows window in about historyPoints
// buckets, in whole minutes.
func historyBucket(window time.Duration) time.Duration {
	b := (window / historyPoints).Truncate(time.Minute)
	if b < time.Minute {
		return time.Minute
	}
	return b
}

func queryDetailHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("queryid"), 10, 64)
		if err != nil {
			jsonError(w, "queryid must be a 64-bit integer", http.StatusBadRequest)
			return
		}
		database, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		if database == "" || database == "all" {
			jsonError(w, "database is required: a queryid belongs to one database",
				http.StatusBadRequest)
			return
		}
		from, to, err := parseWindow(r.URL.Query(), time.Now().UTC(), 24*time.Hour)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rejectUnknownDatabase(w, mgr, database) {
			return
		}
		body := emptyQueryDetail(database, id, from, to)
		sel, _ := resolveSingleDatabaseRequestPool(mgr, database)
		if sel.pool != nil {
			if err := fillQueryDetail(r.Context(), sel.pool, id, body); err != nil {
				internalError(w, r, "read the query page of "+database, err)
				return
			}
		}
		jsonResponse(w, body)
	}
}

func emptyQueryDetail(database string, id int64, from, to time.Time) map[string]any {
	return map[string]any{"database": database, "queryid": strconv.FormatInt(id, 10),
		"query": "", "since": from, "until": to, "coverage": querystore.CoverageNone,
		"samples": 0, "bucket_seconds": int(historyBucket(to.Sub(from)).Seconds()),
		"buckets": []historyBucketJSON{}, "plan_changes": []planChangeJSON{}, "plan": nil,
		"cases": []map[string]any{}, "hints": []map[string]any{},
		"actions": []map[string]any{}}
}

// fillQueryDetail reads the history, statement, plan and links into body.
func fillQueryDetail(ctx context.Context, pool *pgxpool.Pool, id int64,
	body map[string]any) error {
	from, to := body["since"].(time.Time), body["until"].(time.Time)
	h, err := querystore.QueryHistory(ctx, pool, id, from, to, historyBucket(to.Sub(from)))
	if err != nil {
		return err
	}
	body["coverage"], body["samples"] = h.Coverage, h.Samples
	body["buckets"], body["plan_changes"] = historyJSON(h)
	tools := agenttools.New(pool, agenttools.Options{})
	if err := fillStatement(ctx, tools, id, body); err != nil {
		return err
	}
	plans, err := tools.Plans(ctx, []int64{id})
	if err != nil {
		return err
	}
	if p := plans[id]; p != nil {
		body["plan"] = queryPlanJSON{Source: p.Source, CapturedAt: p.CapturedAt,
			TotalCost: p.TotalCost, PlanHash: p.PlanHash, NodeTypes: p.NodeTypes,
			PlanJSON: redactPlanJSON(p.JSON)}
	}
	return fillQueryLinks(ctx, pool, id, body)
}

func historyJSON(h querystore.History) ([]historyBucketJSON, []planChangeJSON) {
	buckets := make([]historyBucketJSON, len(h.Buckets))
	for i, b := range h.Buckets {
		buckets[i] = historyBucketJSON{Start: b.Start, Calls: b.Calls, TotalTimeMs: b.TotalMs,
			MeanTimeMs: b.MeanMs, Rows: b.Rows, EpochBreak: b.EpochBreak}
	}
	markers := make([]planChangeJSON, len(h.Markers))
	for i, m := range h.Markers {
		markers[i] = planChangeJSON{At: m.At, From: m.From, To: m.To}
	}
	return buckets, markers
}

// fillStatement sets the statement's text, or why it is missing.
func fillStatement(ctx context.Context, tools *agenttools.Tools, id int64,
	body map[string]any) error {
	texts, err := tools.Statements(ctx, []int64{id})
	if reason := textUnavailableReason(err); reason != "" {
		body["text_unavailable"] = reason
		return nil
	}
	if err != nil {
		return err
	}
	st, ok := texts[id]
	if !ok {
		body["text_unavailable"] = evictedText
		return nil
	}
	body["query"], body["own_statement"] = st.Query, st.Excluded
	if len(st.Tags) > 0 {
		body["sqlcommenter"] = st.Tags
	}
	return nil
}

// queryLink is one list of links of a query page: its SQL reads the rows
// about $1 (the queryid, or its finding identity queryid:<id>), newest
// first, at most $2.
type queryLink struct {
	key, sql string
	byIdent  bool
}

var queryLinks = []queryLink{
	{key: "cases", byIdent: true, sql: `/* pg_sage */ SELECT id AS finding_id, title,
		severity, status, category, last_seen FROM sage.findings
		WHERE object_identifier = $1 ORDER BY last_seen DESC, id DESC LIMIT $2`},
	{key: "hints", sql: `/* pg_sage */ SELECT id, status, hint_text, symptom, created_at
		FROM sage.query_hints WHERE queryid = $1
		ORDER BY created_at DESC, id DESC LIMIT $2`},
	{key: "actions", byIdent: true, sql: `/* pg_sage */ SELECT a.id, a.action_type,
		a.outcome, a.executed_at, a.finding_id FROM sage.action_log a
		WHERE a.finding_id IN (
			SELECT f.id FROM sage.findings f WHERE f.object_identifier = $1)
		ORDER BY a.executed_at DESC, a.id DESC LIMIT $2`},
}

// fillQueryLinks reads each list of links into body.
func fillQueryLinks(ctx context.Context, pool *pgxpool.Pool, id int64,
	body map[string]any) error {
	for _, l := range queryLinks {
		var arg any = id
		if l.byIdent {
			arg = fmt.Sprintf("queryid:%d", id)
		}
		rows, err := pool.Query(ctx, l.sql, arg, queryLinkLimit)
		if err != nil {
			return fmt.Errorf("read the %s of query %d: %w", l.key, id, err)
		}
		list, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			return fmt.Errorf("read the %s of query %d: %w", l.key, id, err)
		}
		if list == nil {
			list = []map[string]any{}
		}
		body[l.key] = list
	}
	return nil
}
