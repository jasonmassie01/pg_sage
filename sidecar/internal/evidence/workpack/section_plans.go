package workpack

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/planhash"
)

// plans.json: the latest sage.explain_cache plan captured before the
// window's end of each top queryid, as shape only (planhash.Shape) with
// its fingerprint. A full plan with its conditions is in the pack only
// when the build asks for it, and then with every string's SQL literals
// redacted, as the API shows plans to viewers.

// planRow is one captured plan.
type planRow struct {
	qid      int64
	plan     []byte
	source   string
	captured time.Time
}

var plansSQL = `/* pg_sage */ SELECT DISTINCT ON (e.queryid) e.queryid, e.plan_json::text,
       e.source, e.captured_at
  FROM sage.explain_cache e
 WHERE e.queryid = ANY($1::int8[]) AND e.captured_at < $2
 ORDER BY e.queryid, e.captured_at DESC, e.id DESC`

func readPlans(ctx context.Context, tx pgx.Tx, in catalogInput, cat *catalogData) error {
	cat.plans = map[int64]planRow{}
	if len(in.top) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, plansSQL, in.top, in.to)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p planRow
		var text string
		if err := rows.Scan(&p.qid, &text, &p.source, &p.captured); err != nil {
			return err
		}
		p.plan = []byte(text)
		cat.plans[p.qid] = p
	}
	return rows.Err()
}

// topQueryIDs is the window's top queryids by exec time (then queryid),
// at most MaxPlans, from the listed families.
func topQueryIDs(wl Workload) []int64 {
	type q struct {
		id   int64
		exec float64
	}
	var all []q
	for _, fam := range wl.Families {
		for _, x := range fam.QueryIDs {
			id, err := strconv.ParseInt(x.QueryID, 10, 64)
			if err == nil {
				all = append(all, q{id, x.ExecMs})
			}
		}
	}
	sort.Slice(all, func(a, b int) bool {
		if all[a].exec != all[b].exec {
			return all[a].exec > all[b].exec
		}
		return all[a].id < all[b].id
	})
	if len(all) > MaxPlans {
		all = all[:MaxPlans]
	}
	out := make([]int64, len(all))
	for i, x := range all {
		out[i] = x.id
	}
	return out
}

// buildPlans is one fact per top queryid, in their order.
func buildPlans(top []int64, rows map[int64]planRow, conditions bool) Plans {
	out := Plans{Status: valueAbsent, Plans: []PlanFact{}}
	if len(top) > 0 {
		out.Status = relStatusOK
	}
	for _, qid := range top {
		id := strconv.FormatInt(qid, 10)
		f := PlanFact{ID: "plan/query/" + id, QueryID: id, Plan: valueAbsent}
		if row, ok := rows[qid]; ok {
			fillPlan(&f, row, conditions)
		}
		out.Plans = append(out.Plans, f)
	}
	return out
}

func fillPlan(f *PlanFact, row planRow, conditions bool) {
	f.Source, f.CapturedAt = row.source, row.captured.UTC().Format(time.RFC3339Nano)
	shape, err := planhash.Shape(row.plan)
	if err != nil {
		f.Plan = valueUnreadable
		return
	}
	fp, err := planhash.Compute(row.plan)
	if err != nil {
		f.Plan = valueUnreadable
		return
	}
	f.Plan, f.Fingerprint, f.Shape = relStatusOK, fp, json.RawMessage(shape)
	if conditions {
		f.Conditions = redactedPlan(row.plan)
	}
}

// redactedPlan is the plan document with every string's SQL literals
// redacted (llm.RedactSQLLiterals), numbers exact.
func redactedPlan(raw []byte) any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return valueUnreadable
	}
	return redactValue(v)
}

func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return llm.RedactSQLLiterals(x)
	case map[string]any:
		for k, e := range x {
			x[k] = redactValue(e)
		}
	case []any:
		for i, e := range x {
			x[i] = redactValue(e)
		}
	}
	return v
}
