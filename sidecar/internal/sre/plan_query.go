package sre

import (
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// A plan-regression investigation is about one statement: its subject is
// "queryid N" (pg_sage's own plan findings, and specialist callers that
// pass a query_id). Its plan_regressions probe then reads only that
// statement, so the probe's row cap can never hide it behind larger
// regressions of other statements.

const querySubjectPrefix = "queryid "

// QuerySubject is the subject of an investigation about one statement.
func QuerySubject(queryID int64) string {
	return querySubjectPrefix + strconv.FormatInt(queryID, 10)
}

// ParseQuerySubject returns the statement of a QuerySubject; false for any
// other subject (and for queryid 0, which names no statement).
func ParseQuerySubject(subject string) (int64, bool) {
	digits, ok := strings.CutPrefix(subject, querySubjectPrefix)
	if !ok {
		return 0, false
	}
	q, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || q == 0 || strconv.FormatInt(q, 10) != digits {
		return 0, false
	}
	return q, true
}

// scopePlanQuery returns plan with a plan-regression investigation's
// plan_regressions calls scoped to its statement; any other plan is
// returned as is. plan is not modified.
func scopePlanQuery(plan []planStep, inv Investigation) []planStep {
	q, ok := ParseQuerySubject(inv.Subject)
	if inv.TriggerKind != TriggerPlan || !ok || plan == nil {
		return plan
	}
	out := make([]planStep, len(plan))
	for i, st := range plan {
		st.calls = append([]probeCall(nil), st.calls...)
		for j := range st.calls {
			if st.calls[j].id == probes.PlanRegressions {
				st.calls[j].args.QueryID = q
			}
		}
		out[i] = st
	}
	return out
}
