package executor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// Evidence keys the ledger stamps on every standing-gate decision. They
// are reserved: a request's own evidence can never set them.
const (
	budgetKindKey     = "budget_kind"
	rowsRewrittenKey  = "rows_rewritten"
	budgetDetailKey   = "budget_detail"
	budgetReleasedKey = "budget_released"
)

// standingUsage reports the rolling window for the gate's limits, for the
// budget kind of the request (policy.BudgetKindFor) as if the request ran:
//
//   - changes: executed self-initiated actions of that kind, plus
//     authorizations of other requests still in flight (see
//     standingUsageSQL); operator-approved decisions are excluded;
//   - tables: the distinct tables those touched plus the request's own, an
//     index identity ("public.t|btree(c)") counting as its table, so the
//     gate's "TablesInWindow > limit" never admits one table past it;
//   - rows rewritten: recorded rewrites of every kind plus the request's
//     own catalog estimate.
//
// Decisions recorded without a kind, or with an unknown one, charge the
// performance budget (fail closed).
func (e *Executor) standingUsage(
	ctx context.Context, request policy.ActionRequest,
) (policy.LimitUsage, error) {
	var usage policy.LimitUsage
	if e.pool == nil {
		return usage, fmt.Errorf("policy usage requires a database pool")
	}
	requestRows, err := e.estimateRowsRewritten(ctx, request.SQL)
	if err != nil {
		return usage, fmt.Errorf("read policy usage: %w", err)
	}
	targets := request.TargetObjs
	if targets == nil {
		targets = []string{}
	}
	var changesFree, tablesFree, rowsFree *time.Time
	err = e.usageRow(ctx, standingUsageSQL, operatorDecisionIntent, targets,
		string(policy.BudgetKindFor(request)), e.budgetHoldHorizon().Seconds(), request.SQL,
	).Scan(&usage.SelfInitiatedChangesInWindow, &usage.TablesInWindow, &usage.RowsRewritten,
		&changesFree, &tablesFree, &rowsFree)
	if err != nil {
		return usage, fmt.Errorf("read policy usage: %w", err)
	}
	usage.RequestRowsRewritten = requestRows
	usage.RowsRewritten += requestRows
	usage.ChangesFreeAt, usage.TablesFreeAt = timeOrZero(changesFree), timeOrZero(tablesFree)
	usage.RowsFreeAt = timeOrZero(rowsFree)
	return usage, nil
}

func timeOrZero(at *time.Time) time.Time {
	if at == nil {
		return time.Time{}
	}
	return *at
}

// budgetHoldHorizon is how long an execute authorization without an
// action holds its budget slot when nothing releases it (a crash): Apply's
// two bounded stages, the waits and the run.
func (e *Executor) budgetHoldHorizon() time.Duration {
	return 2 * e.applyTimeout()
}

// releaseBudget ends the budget hold of execute authorizations once the
// change they authorized has returned. An executed change stays counted
// through its action_log row; one that ran nothing (admission withheld, a
// lease conflict, a refusal) stops holding a slot at once.
func (e *Executor) releaseBudget(ctx context.Context, decisionIDs ...int64) {
	ids := make([]int64, 0, len(decisionIDs))
	for _, id := range decisionIDs {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	if e.pool == nil || len(ids) == 0 {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := e.pool.Exec(releaseCtx, `/* pg_sage */ UPDATE sage.decision
		SET evidence = evidence || jsonb_build_object('`+budgetReleasedKey+`', now())
		WHERE id = ANY($1) AND verdict = 'execute'
		  AND NOT evidence ? '`+budgetReleasedKey+`'`, ids); err != nil {
		e.logFn("executor", "release the budget hold of decisions %v: %v "+
			"(they hold their slot until the %s horizon)", ids, err, e.budgetHoldHorizon())
	}
}

// stampBudgetEvidence charges a decision to its budget: the kind from the
// typed contract, the gate's row estimate when it read usage, and the
// budget detail of a budget park. Request evidence cannot set these keys.
func stampBudgetEvidence(
	evidence map[string]any, request policy.ActionRequest, decision policy.Decision,
) {
	for _, key := range []string{budgetKindKey, rowsRewrittenKey, budgetDetailKey,
		budgetReleasedKey, "gate_reason"} {
		delete(evidence, key)
	}
	kind := policy.BudgetKindFor(request)
	if decision.BudgetKind == policy.BudgetBypass {
		kind = policy.BudgetBypass // charged to no kind budget
	}
	evidence[budgetKindKey] = string(kind)
	if decision.BudgetKind != "" {
		evidence[rowsRewrittenKey] = decision.RowsRewritten
	}
	if kind == policy.BudgetBypass && decision.Detail != "" {
		evidence[budgetDetailKey] = decision.Detail
	}
	switch decision.Reason {
	case policy.ReasonBlastRadiusExceeded, policy.ReasonRateLimitExceeded,
		policy.ReasonBudgetExceeded:
		if decision.Detail != "" {
			evidence[budgetDetailKey] = decision.Detail
		}
	}
}

// standingUsageSQL reads the window of one budget kind ($3).
//
// executed: self-initiated actions run in the last 24 hours. held: execute
// authorizations of other requests ($5 is the request's SQL, $2 its
// targets) younger than the hold horizon ($4 seconds) whose action has not
// run, that are not released and not followed by an executed authorization
// of the same change; a change authorized twice (before and after its
// waits) is held once. Targets are split at "|": the recommendation
// identity of an index is "<schema>.<table>|<definition>".
const standingUsageSQL = `/* pg_sage */
WITH executed AS (
	SELECT d.target_objects AS targets, al.executed_at AS spent_at, d.evidence
	  FROM sage.action_log al
	  JOIN sage.decision d ON d.id = al.decision_id
	 WHERE al.executed_at > now() - interval '24 hours'
	   AND d.intent <> $1
), held AS (
	SELECT DISTINCT ON (d.evidence->>'proposed_sql', d.target_objects)
	       d.target_objects AS targets, d.created_at AS spent_at, d.evidence
	  FROM sage.decision d
	 WHERE d.verdict = 'execute' AND d.intent <> $1 AND d.risk_tier <> 'read_only'
	   AND d.created_at > now() - make_interval(secs => $4)
	   AND NOT d.evidence ? '` + budgetReleasedKey + `'
	   AND NOT EXISTS (SELECT 1 FROM sage.action_log al WHERE al.decision_id = d.id)
	   AND NOT (COALESCE(d.evidence->>'proposed_sql', '') = $5
	            AND COALESCE(NULLIF(d.target_objects, 'null'::jsonb), '[]'::jsonb)
	                = to_jsonb($2::text[]))
	   AND NOT EXISTS (
	       SELECT 1 FROM sage.decision done
	         JOIN sage.action_log al ON al.decision_id = done.id
	        WHERE done.created_at >= d.created_at AND done.intent <> $1
	          AND done.evidence->>'proposed_sql' IS NOT DISTINCT FROM
	              d.evidence->>'proposed_sql'
	          AND done.target_objects = d.target_objects)
	 ORDER BY d.evidence->>'proposed_sql', d.target_objects, d.created_at
), spent AS (
	SELECT CASE jsonb_typeof(s.targets) WHEN 'array' THEN s.targets
	            ELSE '[]'::jsonb END AS targets,
	       s.spent_at,
	       CASE s.evidence->>'` + budgetKindKey + `' WHEN 'hygiene' THEN 'hygiene'
	            WHEN 'bypass' THEN 'bypass'
	            ELSE 'performance' END AS kind,
	       COALESCE((s.evidence->>'` + rowsRewrittenKey + `')::bigint, 0) AS rows_rewritten
	  FROM (SELECT * FROM executed UNION ALL SELECT * FROM held) s
), mine AS (
	SELECT * FROM spent WHERE kind = $3
), touched AS (
	SELECT btrim(split_part(t.obj, '|', 1)) AS tbl, max(m.spent_at) AS last_at
	  FROM mine m CROSS JOIN LATERAL jsonb_array_elements_text(m.targets) AS t(obj)
	 GROUP BY 1
), tables AS (
	SELECT tbl FROM touched
	UNION
	SELECT btrim(split_part(r.obj, '|', 1)) FROM unnest($2::text[]) AS r(obj)
)
SELECT (SELECT count(*) FROM mine),
       (SELECT count(*) FROM tables WHERE tbl <> ''),
       (SELECT COALESCE(sum(rows_rewritten), 0) FROM spent)::bigint,
       (SELECT min(spent_at) FROM mine) + interval '24 hours',
       (SELECT min(last_at) FROM touched WHERE tbl <> '') + interval '24 hours',
       (SELECT min(spent_at) FROM spent WHERE rows_rewritten > 0) + interval '24 hours'`

// budgetBypassReason records an executed emergency mitigation under the
// reason "budget bypass: <why>" (owner decision 2026-10-03), keeping the
// gate's own reason code in evidence.gate_reason. Withheld decisions keep
// the gate's reason.
func budgetBypassReason(input *ledger.DecisionInput, decision policy.Decision) {
	if decision.BudgetKind != policy.BudgetBypass || decision.Verdict != policy.VerdictExecute {
		return
	}
	input.Evidence["gate_reason"] = string(decision.Reason)
	input.Reason = "budget bypass"
	if strings.HasPrefix(decision.Detail, "budget bypass: ") {
		input.Reason = decision.Detail
	}
}
