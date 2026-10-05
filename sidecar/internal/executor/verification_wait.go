package executor

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/policy"
)

// VerificationWaits finds the changes in flight on a change's objects (one
// change per object): executed actions whose verification has not
// concluded, and changes the gate authorized that have not run yet. It is
// the standing gate's policy.VerificationTracker and the approval card's
// wait source.
type VerificationWaits struct {
	pool    *pgxpool.Pool
	windows func() VerificationWindows
}

// VerificationWaits is the executor's lookup, with its configured windows.
func (e *Executor) VerificationWaits() *VerificationWaits {
	return &VerificationWaits{pool: e.pool, windows: func() VerificationWindows {
		cfg, _, _ := e.policySnapshot()
		return verificationWindowsFor(cfg, e.budgetHoldHorizon())
	}}
}

// PendingVerifications implements policy.VerificationTracker.
func (w *VerificationWaits) PendingVerifications(
	ctx context.Context, req policy.ActionRequest,
) ([]policy.PendingVerification, error) {
	return w.PendingFor(ctx, req.SQL, req.TargetObjs)
}

// PendingFor lists the changes in flight on the objects sql and targets
// name (nothing for a change with no object identity, such as a hint).
func (w *VerificationWaits) PendingFor(
	ctx context.Context, sql string, targets []string,
) ([]policy.PendingVerification, error) {
	guc := changeGUC(sql)
	var relations []string
	if guc == "" {
		relations = changeRelationNames(sql, targets)
	}
	if guc == "" && len(relations) == 0 {
		return nil, nil
	}
	if w == nil || w.pool == nil {
		return nil, errors.New("in-flight verifications: database pool unavailable")
	}
	windows := w.windows()
	db := waitQuerier(ctx, w.pool)
	candidates, err := readInFlight(ctx, db, windows.HoldHorizon, sql, targets)
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	now := time.Now()
	if guc != "" {
		return matchGUC(guc, candidates, now, windows), nil
	}
	return matchTables(ctx, db, sql, relations, candidates, now, windows)
}

// waitQuerier reads in the gate's budget transaction when there is one,
// so concurrent proposals see each other's authorizations under the lock.
func waitQuerier(ctx context.Context, pool *pgxpool.Pool) policy.TargetQuerier {
	if tx, ok := budgetTx(ctx); ok {
		return tx
	}
	return pool
}

// inFlight is one change on the database that may share an object.
type inFlight struct {
	actionID, decisionID int64
	sql, rollback        string
	targets              []string
	at                   time.Time
}

// inFlightSQL reads every change in flight ($1: the hold horizon in
// seconds; $2, $3: the request's SQL and targets, whose own earlier
// authorization is not a different change):
//
//   - executed actions still being watched (idx_action_log_outcome);
//   - executed actions whose verdict is pending (idx_action_outcome_pending)
//     unless they were rolled back, reverted or failed;
//   - self-initiated execute authorizations ($4 is the operator intent)
//     younger than the hold horizon whose action has not been recorded and
//     that were not released (idx_decision_created). An operator's change
//     runs at once and holds its object through its action row.
const inFlightSQL = `/* pg_sage */
SELECT al.id, 0::bigint, al.sql_executed, COALESCE(al.rollback_sql, ''),
       '{}'::text[], al.executed_at
  FROM sage.action_log al
 WHERE al.outcome IN ('monitoring', 'pending', 'interrupted', 'rolling_back')
UNION
SELECT al.id, 0::bigint, al.sql_executed, COALESCE(al.rollback_sql, ''),
       '{}'::text[], al.executed_at
  FROM sage.action_outcome ao
  JOIN sage.action_log al ON al.id = ao.action_log_id
 WHERE ao.verdict = 'pending'
   AND al.outcome NOT IN ('rolled_back', 'reverted', 'failed')
UNION ALL
SELECT 0::bigint, d.id, COALESCE(d.evidence->>'proposed_sql', ''), '',
       ARRAY(SELECT jsonb_array_elements_text(CASE jsonb_typeof(d.target_objects)
             WHEN 'array' THEN d.target_objects ELSE '[]'::jsonb END)),
       d.created_at
  FROM sage.decision d
 WHERE d.verdict = 'execute' AND d.risk_tier <> 'read_only' AND d.intent <> $4
   AND d.created_at > now() - make_interval(secs => $1)
   AND NOT d.evidence ? '` + budgetReleasedKey + `'
   AND NOT EXISTS (SELECT 1 FROM sage.action_log al WHERE al.decision_id = d.id)
   AND NOT (COALESCE(d.evidence->>'proposed_sql', '') = $2
            AND COALESCE(NULLIF(d.target_objects, 'null'::jsonb), '[]'::jsonb)
                = to_jsonb($3::text[]))`

func readInFlight(ctx context.Context, db policy.TargetQuerier, hold time.Duration,
	sql string, targets []string) ([]inFlight, error) {
	if targets == nil {
		targets = []string{}
	}
	rows, err := db.Query(ctx, inFlightSQL, hold.Seconds(), sql, targets,
		operatorDecisionIntent)
	if err != nil {
		return nil, fmt.Errorf("read in-flight verifications: %w", err)
	}
	defer rows.Close()
	var out []inFlight
	for rows.Next() {
		var c inFlight
		if err := rows.Scan(&c.actionID, &c.decisionID, &c.sql, &c.rollback, &c.targets,
			&c.at); err != nil {
			return nil, fmt.Errorf("read in-flight verifications: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read in-flight verifications: %w", err)
	}
	return out, nil
}

// pending is the wait c imposes on object: an action until its verdict
// is due and its hard deadline; an unrun authorization for its hold.
func (c inFlight) pending(object string, now time.Time,
	w VerificationWindows) policy.PendingVerification {
	p := policy.PendingVerification{ActionID: c.actionID, DecisionID: c.decisionID,
		Object: object}
	if c.actionID > 0 {
		p.Until, p.HardDeadline = waitTimes(c.sql, c.at, now, w)
		p.Release = waitRelease(c.sql)
	} else {
		p.Until = c.at.Add(w.HoldHorizon)
		p.HardDeadline = p.Until
	}
	return p
}

func matchGUC(guc string, candidates []inFlight, now time.Time,
	w VerificationWindows) []policy.PendingVerification {
	var out []policy.PendingVerification
	for _, c := range candidates {
		if changeGUC(c.sql) == guc || changeGUC(c.rollback) == guc {
			out = append(out, c.pending("guc:"+guc, now, w))
		}
	}
	return out
}

// matchTables resolves the request's relations and every candidate's (its
// statement, its targets and its rollback: a dropped index's table is
// named by the definition the rollback re-creates) in one catalog read,
// and matches their object keys.
func matchTables(ctx context.Context, db policy.TargetQuerier, sql string,
	relations []string, candidates []inFlight, now time.Time, w VerificationWindows,
) ([]policy.PendingVerification, error) {
	names := append([]string(nil), relations...)
	byCandidate := make([][]string, len(candidates))
	for i, c := range candidates {
		byCandidate[i] = append(changeRelationNames(c.sql, c.targets),
			changeRelationNames(c.rollback, nil)...)
		names = append(names, byCandidate[i]...)
	}
	tables, err := policy.ResolveChangeTables(ctx, db, names)
	if err != nil {
		return nil, fmt.Errorf("in-flight verifications: %w", err)
	}
	wanted := objectKeys(tables, relations, partitionScoped(sql))
	var out []policy.PendingVerification
	for i, c := range candidates {
		scoped := partitionScoped(c.sql) || partitionScoped(c.rollback)
		for _, key := range objectKeys(tables, byCandidate[i], scoped) {
			if slices.Contains(wanted, key) {
				out = append(out, c.pending(key, now, w))
				break
			}
		}
	}
	return out, nil
}

// objectKeys are the objects names change: each table ("table:<name>")
// and, for a partition-scoped change, its partition tree
// ("partition_tree:<root>"), so a change to a partitioned table, a
// partition or one of their indexes meets another on the same tree.
func objectKeys(tables map[string]policy.ChangeTable, names []string,
	scoped bool) []string {
	var keys []string
	for _, name := range names {
		table, ok := tables[name]
		if !ok {
			continue
		}
		keys = append(keys, "table:"+table.Table)
		if scoped && table.Root != "" {
			keys = append(keys, "partition_tree:"+table.Root)
		}
	}
	return keys
}
