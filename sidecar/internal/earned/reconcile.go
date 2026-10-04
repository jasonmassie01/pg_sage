package earned

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HandoffKeyPrefix starts the action-queue identity of an L2 handoff:
// autonomy:<family>:<class>:<target>.
const HandoffKeyPrefix = "autonomy:"

// HandoffKey is the action-queue identity of an L2 handoff.
func HandoffKey(f Family, c ActionClass, targets []string) string {
	return HandoffKeyPrefix + string(f) + ":" + string(c) + ":" + strings.Join(targets, ",")
}

// AutoExecution is an L3 action pg_sage executed on its own.
type AutoExecution struct {
	Database    string      `json:"database"`
	ActionLogID int64       `json:"action_log_id"`
	Family      Family      `json:"family"`
	Class       ActionClass `json:"class"`
	SQL         string      `json:"sql"`
	ExecutedAt  time.Time   `json:"executed_at"`
}

// Notifier tells a human about an L3 auto-execution.
type Notifier interface {
	NotifyAutonomous(ctx context.Context, a AutoExecution) error
}

// UnverifiedAfter is how long after execution an action without any
// verification is recorded as unverified (P0-6): the post-action checks
// write their verdict well within it, so none is coming.
const UnverifiedAfter = 24 * time.Hour

// ReconcileResult counts one reconciliation pass.
type ReconcileResult struct {
	Recorded int `json:"recorded"`
	Notified int `json:"notified"`
	Pending  int `json:"pending"`
	Skipped  int `json:"skipped"`
	// SelfRecorded counts the new trust evidence of self-initiated
	// actions and operator rejections; Demoted the demotions it caused.
	SelfRecorded int `json:"self_recorded"`
	Demoted      int `json:"demoted"`
	// ShadowRecorded counts the new shadow evidence (roadmap 1.4).
	ShadowRecorded int `json:"shadow_recorded"`
}

// Reconciler turns one monitored database's executed family actions into
// ledger outcomes: approved L2 handoffs and L3 auto-executions, matched
// to their action_log row and verification verdict; and its
// self-initiated actions' verdicts, operator rollbacks and rejections
// into trust evidence (selfinit_reconcile.go).
type Reconciler struct {
	svc      *Service
	pool     *pgxpool.Pool
	database string
	notifier Notifier
	lookback time.Duration
}

// NewReconciler reads monitored (the database named database).
func NewReconciler(svc *Service, monitored *pgxpool.Pool, database string,
	notifier Notifier) *Reconciler {
	return &Reconciler{svc: svc, pool: monitored, database: database, notifier: notifier,
		lookback: 30 * 24 * time.Hour}
}

// executed is one executed family action in the monitored database.
// settled reports that it ran more than UnverifiedAfter ago.
type executed struct {
	family       Family
	class        ActionClass
	level        Level
	actionLogID  int64
	outcome      string
	verification string
	verdict      string // sage.action_outcome verdict (Phase 1.3), "" when none
	settled      bool
	sql          string
	at           time.Time
}

// RunOnce records every decided outcome not yet recorded and notifies
// each L3 execution once. A failed notification is retried next pass.
func (r *Reconciler) RunOnce(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult
	if r == nil || r.svc == nil || r.pool == nil || strings.TrimSpace(r.database) == "" {
		return res, fmt.Errorf("%w: reconciler needs a ledger, a database and its pool",
			ErrUnavailable)
	}
	if err := r.svc.store.checkDatabase(r.database); err != nil {
		return res, fmt.Errorf("reconcile %s: %w", r.database, err)
	}
	handoffs, skipped, err := r.handoffs(ctx)
	if err != nil {
		return res, err
	}
	res.Skipped = skipped
	autos, skipped, err := r.autoExecutions(ctx)
	if err != nil {
		return res, err
	}
	res.Skipped += skipped
	notifyErr := r.notifyAll(ctx, autos, &res)
	for _, x := range append(handoffs, autos...) {
		if err := r.record(ctx, x, &res); err != nil {
			return res, err
		}
	}
	selfErr := r.selfInitiated(ctx, &res)
	return res, errors.Join(notifyErr, selfErr, r.shadowEvidence(ctx, &res))
}

func (r *Reconciler) record(ctx context.Context, x executed, res *ReconcileResult) error {
	result, decided := classifyWithVerdict(x.outcome, x.verification, x.verdict, x.settled)
	if !decided {
		res.Pending++
		return nil
	}
	inserted, err := r.svc.recordOutcome(ctx, Outcome{Database: r.database,
		ActionLogID: x.actionLogID, Family: x.family, Class: x.class, Level: x.level,
		Result: result, Source: SourceExecutor, Actor: ActorPgSage,
		Detail: fmt.Sprintf("action_log %s, verification %q, verdict %q", x.outcome,
			x.verification, x.verdict)})
	if inserted {
		res.Recorded++
	}
	return err
}

// classifyOutcome maps an action's outcome and verification verdict to a
// ledger result; decided is false while a verdict may still come. Only a
// completed verification is a verified recovery (P0-6): a success without
// one is unverified once settled, and an unverifiable one at once.
func classifyOutcome(outcome, verification string, settled bool) (string, bool) {
	switch {
	case outcome == "rolled_back" || outcome == "reverted" ||
		outcome == "rollback_failed" || verification == "revert":
		return ResultHarmful, true
	case outcome == "failed" || verification == "failed":
		return ResultNotRecovered, true
	case outcome != "success":
		return "", false
	case verification == "success":
		return ResultVerifiedRecovery, true
	case verification == "unverifiable" || (verification == "" && settled):
		return ResultUnverified, true
	}
	return "", false
}

const handoffSQL = `/* pg_sage */ SELECT q.identity_key, l.id, l.outcome,
	COALESCE(v.verdict, ''), COALESCE(o.verdict, ''),
	l.executed_at < now() - make_interval(secs => $2::double precision),
	l.sql_executed, l.executed_at
	FROM sage.action_queue q
	JOIN sage.action_log l ON l.id = q.action_log_id
	LEFT JOIN sage.verification v ON v.id = l.verification_id
	LEFT JOIN sage.action_outcome o ON o.action_log_id = l.id
	WHERE q.identity_key LIKE 'autonomy:%'
	  AND l.executed_at > now() - make_interval(secs => $1::double precision)
	ORDER BY l.id LIMIT 1000`

// handoffs reads executed L2 handoffs; malformed keys are skipped.
func (r *Reconciler) handoffs(ctx context.Context) ([]executed, int, error) {
	rows, err := r.pool.Query(ctx, handoffSQL, r.lookback.Seconds(),
		UnverifiedAfter.Seconds())
	if err != nil {
		return nil, 0, fmt.Errorf("read executed autonomy handoffs: %w", err)
	}
	defer rows.Close()
	var out []executed
	skipped := 0
	for rows.Next() {
		var key string
		x := executed{level: L2}
		if err := rows.Scan(&key, &x.actionLogID, &x.outcome, &x.verification, &x.verdict,
			&x.settled, &x.sql, &x.at); err != nil {
			return nil, 0, fmt.Errorf("scan autonomy handoff: %w", err)
		}
		var ok bool
		if x.family, x.class, ok = parseHandoffKey(key); !ok {
			skipped++
			r.svc.cfg.logf("autonomy: skip handoff %d with malformed key %q on %s",
				x.actionLogID, key, r.database)
			continue
		}
		out = append(out, x)
	}
	return out, skipped, rows.Err()
}

func parseHandoffKey(key string) (Family, ActionClass, bool) {
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 || parts[0]+":" != HandoffKeyPrefix || parts[3] == "" {
		return "", "", false
	}
	f, c := Family(parts[1]), ActionClass(parts[2])
	return f, c, KnownFamily(f) && knownClass(c)
}

// autoExecutionSQL reads every self-initiated family action the gate let
// execute: L3 auto-executions (reason autonomy_l3) and mandatory deadline
// overrides (any other self-initiated reason; the ledger did not restrict
// them). Operator approvals are the handoff path's.
const autoExecutionSQL = `/* pg_sage */ SELECT d.evidence->>'incident_family',
	COALESCE(d.evidence->>'autonomy_class', ''), d.reason, l.id, l.outcome,
	COALESCE(v.verdict, ''), COALESCE(o.verdict, ''),
	l.executed_at < now() - make_interval(secs => $2::double precision),
	l.sql_executed, l.executed_at
	FROM sage.decision d
	JOIN sage.action_log l ON l.decision_id = d.id
	LEFT JOIN sage.verification v ON v.id = l.verification_id
	LEFT JOIN sage.action_outcome o ON o.action_log_id = l.id
	WHERE d.verdict = 'execute' AND d.evidence ? 'incident_family'
	  AND d.reason <> 'operator_approved'
	  AND l.executed_at > now() - make_interval(secs => $1::double precision)
	ORDER BY l.id LIMIT 1000`

// autoExecutions reads executed self-initiated family actions: L3 ones at
// L3, mandatory deadline overrides at L1 (never promotion evidence; a
// harmful one is still a family regression).
func (r *Reconciler) autoExecutions(ctx context.Context) ([]executed, int, error) {
	rows, err := r.pool.Query(ctx, autoExecutionSQL, r.lookback.Seconds(),
		UnverifiedAfter.Seconds())
	if err != nil {
		return nil, 0, fmt.Errorf("read autonomous executions: %w", err)
	}
	defer rows.Close()
	var out []executed
	skipped := 0
	for rows.Next() {
		var family, class, reason string
		x := executed{level: L1}
		if err := rows.Scan(&family, &class, &reason, &x.actionLogID, &x.outcome,
			&x.verification, &x.verdict, &x.settled, &x.sql, &x.at); err != nil {
			return nil, 0, fmt.Errorf("scan autonomous execution: %w", err)
		}
		x.family, x.class = Family(family), ActionClass(class)
		if reason == "autonomy_l3" {
			x.level = L3
		}
		if !KnownFamily(x.family) || !knownClass(x.class) {
			skipped++
			continue
		}
		out = append(out, x)
	}
	return out, skipped, rows.Err()
}

// notifyAll notifies each L3 execution not yet notified, then records
// it; a failed notification is not recorded, so the next pass retries.
func (r *Reconciler) notifyAll(ctx context.Context, autos []executed,
	res *ReconcileResult) error {
	var errs []error
	for _, x := range autos {
		if x.level != L3 {
			continue // a mandatory deadline override, not an autonomous action
		}
		done, err := r.svc.store.autoExecutedRecorded(ctx, x.actionLogID)
		if err != nil {
			return err
		}
		if done {
			continue
		}
		a := AutoExecution{Database: r.database, ActionLogID: x.actionLogID,
			Family: x.family, Class: x.class, SQL: x.sql, ExecutedAt: x.at.UTC()}
		if r.notifier != nil {
			if err := r.notifier.NotifyAutonomous(ctx, a); err != nil {
				errs = append(errs, fmt.Errorf("notify L3 action %d on %s: %w",
					x.actionLogID, r.database, err))
				continue
			}
		}
		if err := r.svc.store.appendEvent(ctx, r.svc.store.pool, Event{Family: x.family,
			Class: x.class, Type: EventAutoExecuted, Actor: ActorPgSage,
			Reason:      "executed at L3; a human was notified",
			ActionLogID: x.actionLogID, At: r.svc.now()}); err != nil {
			return err
		}
		res.Notified++
	}
	return errors.Join(errs...)
}
