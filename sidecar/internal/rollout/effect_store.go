package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/verify"
)

// effectReadTimeout bounds each effect read on a fleet database.
const effectReadTimeout = 10 * time.Second

// effectDB is a fleet database's pool: pg_sage's own schema on it.
type effectDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PoolEffects reads a fleet database's effects from its own sage schema
// (lab-03b): read-only, bounded statements, and the tier-4 ladder write.
type PoolEffects struct{ db effectDB }

// NewPoolEffects reads effects on db.
func NewPoolEffects(db effectDB) PoolEffects { return PoolEffects{db: db} }

// normalizedQuerySQL normalizes a statement's text the same way on every
// database (case, whitespace, a trailing semicolon): queryids differ
// across databases, the normalized pg_stat_statements text does not.
func normalizedQuerySQL(col string) string {
	return `lower(regexp_replace(rtrim(btrim(` + col + `), ';'), '\s+', ' ', 'g'))`
}

// actionEffectSQL reads an action's v2 record (the acted verdict's, or the
// shadow one in other_evidence), its window and its target queryids.
const actionEffectSQL = `/* pg_sage */ SELECT COALESCE(o.evidence->'v2',
	o.other_evidence->'v2'), COALESCE(o.window_start, l.executed_at),
	COALESCE(o.window_end, o.decided_at, now()),
	COALESCE(o.predicted->'target_queryids', l.before_state->'target_queryids')
FROM sage.action_outcome o JOIN sage.action_log l ON l.id = o.action_log_id
WHERE o.action_log_id = $1`

// ActionEffect reads the action's own v2 verdict. No outcome row, or no
// v2 record, is no number (not an error).
func (p PoolEffects) ActionEffect(ctx context.Context, actionLogID int64) (MemberEffect,
	error) {
	ctx, cancel := context.WithTimeout(ctx, effectReadTimeout)
	defer cancel()
	var v2, targets []byte
	var eff MemberEffect
	err := p.db.QueryRow(ctx, actionEffectSQL, actionLogID).Scan(&v2, &eff.From, &eff.To,
		&targets)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberEffect{Reason: "no recorded outcome for the action"}, nil
	}
	if err != nil {
		return MemberEffect{}, fmt.Errorf("read the outcome of action %d: %w", actionLogID,
			err)
	}
	eff.From, eff.To = eff.From.UTC(), eff.To.UTC()
	if err := decodeV2(v2, &eff); err != nil || !eff.Measured {
		return eff, err
	}
	// Target queries that cannot be read leave the v2 number deciding (fail
	// closed: a measured effect is never turned into flag only), with no
	// fleet control and the reason said.
	ids, err := queryIDs(targets)
	if err != nil {
		eff.Reason = err.Error()
		return eff, nil
	}
	if eff.Queries, err = p.queryTexts(ctx, ids, eff.To); err != nil {
		eff.Queries = nil
		eff.Reason = fmt.Sprintf("read the target queries of action %d: %v", actionLogID,
			err)
	}
	return eff, nil
}

// decodeV2 fills eff from a stored v2 record (judge_v2.go's v2Record).
func decodeV2(raw []byte, eff *MemberEffect) error {
	if len(raw) == 0 || string(raw) == "null" {
		eff.Reason = "no v2 verdict recorded for the action"
		return nil
	}
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		var v any
		_ = json.Unmarshal(raw, &v)
		eff.Malformed = true
		eff.Reason = fmt.Sprintf("the v2 record is a %s, not a JSON object", jsonKind(v))
		return nil
	}
	if measured, _ := rec["measured"].(bool); !measured {
		reason, _ := rec["attribution_reason"].(string)
		eff.Reason = "v2 did not measure the effect: " + reason
		return nil
	}
	nums := map[string]*float64{"effect_pct": &eff.EffectPct, "ci_low_pct": &eff.CILowPct,
		"ci_high_pct": &eff.CIHighPct}
	for _, key := range []string{"effect_pct", "ci_low_pct", "ci_high_pct"} {
		f, ok := rec[key].(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			eff.Malformed = true
			eff.Reason = "the v2 record has no finite " + key
			return nil
		}
		*nums[key] = f
	}
	eff.Measured = true
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case []any:
		return "array"
	case float64:
		return "number"
	case bool:
		return "boolean"
	}
	return "value"
}

// queryIDs decodes a target_queryids array (integers or integer strings)
// exactly: queryids span int64, past float64's 53 exact bits.
func queryIDs(raw []byte) ([]int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("the target queryids are not a JSON array: %w", err)
	}
	out := make([]int64, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case json.Number:
			id, err := strconv.ParseInt(v.String(), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("target queryid %s is not a number", v)
			}
			out = append(out, id)
		case string:
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("target queryid %q is not a number", v)
			}
			out = append(out, id)
		default:
			return nil, fmt.Errorf("target queryid %v is not a number", item)
		}
	}
	return out, nil
}

// latestQueriesSQL is the newest queries snapshot at or before $1.
var latestQueriesSQL = `SELECT ` + snapstore.DataSQL("s") + ` AS doc
	FROM sage.snapshots s WHERE {db:s} AND s.category = 'queries' AND s.collected_at <= $1
	ORDER BY s.collected_at DESC LIMIT 1`

// queryRecordsSQL expands a queries document into (queryid, query).
const queryRecordsSQL = ` CROSS JOIN LATERAL jsonb_to_recordset(CASE
	WHEN jsonb_typeof(d.doc) = 'array' THEN d.doc ELSE '[]'::jsonb END)
	AS r(queryid int8, query text) WHERE r.queryid IS NOT NULL AND r.query IS NOT NULL`

var queryTextsSQL = `/* pg_sage */ SELECT DISTINCT ` + normalizedQuerySQL("r.query") +
	` FROM (` + latestQueriesSQL + `) d` + queryRecordsSQL +
	` AND r.queryid = ANY($2::int8[]) ORDER BY 1`

// queryTexts are the normalized texts of ids in the newest queries
// snapshot at or before `at`.
func (p PoolEffects) queryTexts(ctx context.Context, ids []int64, at time.Time) ([]string,
	error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := histstore.Resolve(p.db).Query(ctx, queryTextsSQL, at, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// queryChangeSQL sums the matched queries' blocks and calls before and
// after $2 over [$2 - ($3 - $2), $3), leaving out epoch-broken hours.
var queryChangeSQL = `/* pg_sage */ WITH ids AS (SELECT DISTINCT r.queryid FROM (` +
	latestQueriesSQL + `) d` + queryRecordsSQL + ` AND ` + normalizedQuerySQL("r.query") +
	` = ANY($4::text[]))
SELECT COALESCE(sum(h.shared_blks_hit + h.shared_blks_read) FILTER (WHERE h.hour < $2), 0),
	COALESCE(sum(h.calls) FILTER (WHERE h.hour < $2), 0),
	COALESCE(sum(h.shared_blks_hit + h.shared_blks_read) FILTER (WHERE h.hour >= $2), 0),
	COALESCE(sum(h.calls) FILTER (WHERE h.hour >= $2), 0)
FROM sage.query_rollup_hourly h
WHERE {db:h} AND h.queryid IN (SELECT queryid FROM ids) AND NOT h.epoch_break
	AND h.hour >= $2::timestamptz - ($3::timestamptz - $2::timestamptz) AND h.hour < $3`

// QueryChange is the change of the queries' blocks per call over [from,
// to) against the as-long period before from, matched by normalized text.
func (p PoolEffects) QueryChange(ctx context.Context, queries []string, from,
	to time.Time) (float64, bool, error) {
	if !from.Before(to) {
		return 0, false, fmt.Errorf("query change window %s..%s is empty",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if len(queries) == 0 {
		return 0, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, effectReadTimeout)
	defer cancel()
	var beforeBlocks, beforeCalls, afterBlocks, afterCalls float64
	err := histstore.Resolve(p.db).QueryRow(ctx, queryChangeSQL, to, from, to,
		queries).Scan(&beforeBlocks, &beforeCalls, &afterBlocks, &afterCalls)
	if err != nil {
		return 0, false, fmt.Errorf("read the target queries' query history: %w", err)
	}
	if beforeCalls <= 0 || afterCalls <= 0 || beforeBlocks <= 0 {
		return 0, false, nil
	}
	before, after := beforeBlocks/beforeCalls, afterBlocks/afterCalls
	return (after/before - 1) * 100, true, nil
}

// RecordFleetLadder sets the action's tier-4 ladder record.
func (p PoolEffects) RecordFleetLadder(ctx context.Context, actionLogID int64,
	rec verify.LadderRecord) error {
	ctx, cancel := context.WithTimeout(ctx, effectReadTimeout)
	defer cancel()
	_, err := verify.RecordFleetForAction(ctx, p.db, actionLogID, rec)
	return err
}
