package outcomes

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier runs the ledger's one read (*pgxpool.Pool, pgx.Conn or a Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Query is a ledger read: the changes executed at or after Now - Since,
// newest first. Verdict ("" all) and Evidence ("" all; a decision's or a
// credited incident's evidence id) filter them; Limit rows are returned.
type Query struct {
	Since    time.Duration
	Now      time.Time
	Verdict  string
	Evidence string
	Limit    int
}

// ledgerSQL reads every column of a row in one statement, so a verdict
// recorded while it runs is seen whole or not at all. A failed action
// changed nothing and is not a row. $1 cutoff, $2 evidence id ("" any),
// $3 row cap.
const ledgerSQL = `/* pg_sage */ SELECT al.id, al.executed_at, al.action_type,
 al.outcome, COALESCE(al.rollback_sql, ''), al.toil_minutes_saved::float8,
 COALESCE(NULLIF(q.reason, ''), NULLIF(f.title, ''), NULLIF(d.reason, ''),
   COALESCE(al.justification, '')),
 (al.approved_by IS NOT NULL OR q.decided_by IS NOT NULL), al.principal_id IS NOT NULL,
 COALESCE(d.evidence_id, ''), al.finding_id,
 COALESCE(o.verdict, ''), o.predicted, o.observed, COALESCE(o.reason, ''),
 st.plan_id, COALESCE(st.verdict, ''), st.expected_pct::float8,
 COALESCE(st.expected_direction, ''), COALESCE(st.metric, ''),
 COALESCE(st.expected_source, ''), COALESCE(st.pack_hash, '')
 FROM sage.action_log al
 LEFT JOIN sage.action_outcome o ON o.action_log_id = al.id
 LEFT JOIN sage.findings f ON f.id = al.finding_id
 LEFT JOIN LATERAL (SELECT q.reason, q.decided_by FROM sage.action_queue q
   WHERE q.action_log_id = al.id ORDER BY q.id DESC LIMIT 1) q ON true
 LEFT JOIN LATERAL (
   (SELECT d.evidence_id, d.reason FROM sage.decision d WHERE d.id = al.decision_id)
   UNION ALL
   (SELECT d.evidence_id, d.reason FROM sage.decision d WHERE d.action_log_id = al.id
     ORDER BY d.id DESC LIMIT 1)
   LIMIT 1) d ON true
 LEFT JOIN (SELECT DISTINCT ON (ps.action_log_id) ps.action_log_id, ps.plan_id,
     ps.verdict, ps.expected_pct, ps.expected_direction, ps.metric, ps.expected_source,
     p.pack_hash
   FROM sage.plan_step ps JOIN sage.plan p ON p.id = ps.plan_id
   WHERE ps.action_log_id IS NOT NULL
   ORDER BY ps.action_log_id, ps.id DESC) st ON st.action_log_id = al.id
 WHERE al.executed_at >= $1 AND al.outcome <> 'failed'
   AND ($2 = '' OR d.evidence_id = $2 OR EXISTS (SELECT 1 FROM sage.incident_avoided i
     WHERE i.action_log_id = al.id AND i.evidence_id = $2))
 ORDER BY al.executed_at DESC, al.id DESC
 LIMIT $3`

// Read reads database's ledger for q. An error names the database.
func Read(ctx context.Context, db Querier, database string, q Query) (Result, error) {
	rows, err := db.Query(ctx, ledgerSQL, cutoff(q.Now, q.Since), q.Evidence, MaxLimit+1)
	if err != nil {
		return Result{}, fmt.Errorf("read outcomes: %s: %w", database, err)
	}
	all, err := scanRows(rows, database)
	if err != nil {
		return Result{}, fmt.Errorf("read outcomes: %s: %w", database, err)
	}
	res := Result{Since: formatHours(q.Since), Errors: []ReadError{}}
	if len(all) > MaxLimit {
		all, res.Truncated = all[:MaxLimit], true
	}
	all = filterVerdict(all, q.Verdict)
	res.Summary = Summarize(all)
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if len(all) > limit {
		all = all[:limit]
	}
	res.Rows = all
	return res, nil
}

// scanned is one result row before it becomes a Row.
type scanned struct {
	outcome, rollbackSQL, why, outcomeVerdict, reason, stepVerdict string
	saved                                                          *float64
	approved, agent                                                bool
	predicted, observed                                            []byte
	step                                                           stepExpect
}

func scanRows(rows pgx.Rows, database string) ([]Row, error) {
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		var r Row
		var s scanned
		if err := rows.Scan(&r.ActionID, &r.ExecutedAt, &r.ActionType, &s.outcome,
			&s.rollbackSQL, &s.saved, &s.why, &s.approved, &s.agent, &r.EvidenceID,
			&r.FindingID, &s.outcomeVerdict, &s.predicted, &s.observed, &s.reason,
			&r.PlanID, &s.stepVerdict, &s.step.pct, &s.step.direction, &s.step.metric,
			&s.step.source, &r.PackHash); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, build(r, s, database))
	}
	return out, rows.Err()
}

func build(r Row, s scanned, database string) Row {
	r.Database, r.What, r.Why = database, Label(r.ActionType), s.why
	r.ExecutedAt = r.ExecutedAt.UTC()
	r.Verdict = verdictOf(s.outcomeVerdict, s.stepVerdict, s.outcome)
	r.RollbackState = rollbackStateOf(s.outcome, s.rollbackSQL)
	r.Measured, r.MeasuredReason = measuredOf(r.Verdict, s.observed, s.reason)
	r.Expected = expectedOf(s.predicted, s.step)
	switch {
	case r.PlanID != nil:
		r.Origin = OriginPlan
	case s.approved:
		r.Origin = OriginApproved
	case s.agent:
		r.Origin = OriginAgent
	default:
		r.Origin = OriginAutonomous
	}
	if s.saved != nil {
		h := round(*s.saved/60, 2)
		r.SavedHours = &h
	}
	return r
}
