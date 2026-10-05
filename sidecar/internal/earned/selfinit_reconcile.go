package earned

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The reconciler's self-initiated pass (roadmap 1.2) reads, in the
// monitored database, what happened to pg_sage's own actions since the
// last pass and records it as trust evidence for (database, family,
// class): the verdict of each action (sage.action_outcome), each action
// an operator rolled back, and each proposal an operator rejected. A
// demerit demotes the pair one level and an operator is told. Each kind
// keeps a cursor (sage.trust_ledger_state); rows at the cursor are read
// again and de-duplicated by the outcome table's unique indexes.

// selfBatch bounds one kind's read per pass.
const selfBatch = 500

// selfCursors are the last observation times read, per kind.
type selfCursors struct{ verdict, rollback, rejection *time.Time }

// selfFact is one observed action or proposal with the outcome it makes.
type selfFact struct {
	outcome Outcome
	at      time.Time
}

func (r *Reconciler) selfInitiated(ctx context.Context, res *ReconcileResult) error {
	cur, err := r.svc.store.cursors(ctx)
	if err != nil {
		return err
	}
	var notifyErrs []error
	for _, kind := range []struct {
		read   func(context.Context, *time.Time) ([]selfFact, *time.Time, error)
		cursor **time.Time
	}{
		{r.verdictFacts, &cur.verdict}, {r.rollbackFacts, &cur.rollback},
		{r.rejectionFacts, &cur.rejection},
	} {
		facts, last, err := kind.read(ctx, *kind.cursor)
		if err != nil {
			return err
		}
		errs, err := r.recordSelf(ctx, facts, res)
		if err != nil {
			return err
		}
		notifyErrs = append(notifyErrs, errs...)
		if last != nil {
			*kind.cursor = last
		}
	}
	if err := r.svc.store.saveCursors(ctx, cur); err != nil {
		return err
	}
	return errors.Join(notifyErrs...)
}

// recordSelf records each fact; a demotion it causes is notified. It
// returns the notification failures apart from a recording failure.
func (r *Reconciler) recordSelf(ctx context.Context, facts []selfFact,
	res *ReconcileResult) ([]error, error) {
	var notifyErrs []error
	for _, f := range facts {
		inserted, d, err := r.svc.recordOutcomeDemoting(ctx, f.outcome)
		if err != nil {
			return notifyErrs, err
		}
		if inserted {
			res.SelfRecorded++
		}
		if d == nil {
			continue
		}
		res.Demoted++
		r.svc.cfg.logf("autonomy: %s", d.Detail)
		if n, ok := r.notifier.(DemotionNotifier); ok {
			if err := n.NotifyDemotion(ctx, *d); err != nil {
				notifyErrs = append(notifyErrs, fmt.Errorf("notify demotion of %s/%s on %s: %w",
					d.Family, d.Class, d.Database, err))
			}
		}
	}
	return notifyErrs, nil
}

const verdictFactsSQL = `/* pg_sage */ SELECT l.id, o.action_class, o.verdict, l.outcome,
	o.decided_at, COALESCE(d.reason, ''), COALESCE(d.evidence ? 'approved_by', false)
	FROM sage.action_outcome o
	JOIN sage.action_log l ON l.id = o.action_log_id
	LEFT JOIN sage.decision d ON d.id = l.decision_id
	WHERE o.decided_at >= COALESCE($1::timestamptz,
	                               now() - make_interval(secs => $2::double precision))
	  AND o.verdict IN ('improved', 'neutral', 'regressed', 'insufficient_evidence',
	                    'unverifiable')
	  AND NOT COALESCE(d.evidence ? 'incident_family', false)
	ORDER BY o.decided_at, l.id LIMIT $3`

// verdictFacts are the decided verdicts of self-initiated actions.
func (r *Reconciler) verdictFacts(ctx context.Context, cursor *time.Time) ([]selfFact,
	*time.Time, error) {
	return r.readFacts(ctx, "read action verdicts", verdictFactsSQL, cursor,
		func(rows pgx.Rows) (selfFact, bool, error) {
			var id int64
			var outcomeClass, verdict, lifecycle, reason string
			var approved bool
			var at time.Time
			if err := rows.Scan(&id, &outcomeClass, &verdict, &lifecycle, &at, &reason,
				&approved); err != nil {
				return selfFact{}, false, err
			}
			c := ClassForOutcomeClass(outcomeClass)
			result, ok := selfResult(SelfFamilyFor(c), verdict, lifecycle)
			o := r.selfOutcome(id, c, actionLevel(reason, approved), at)
			o.Result, o.Source, o.Verdict = result, SourceExecutor, verdict
			o.Detail = fmt.Sprintf("verdict %s, lifecycle %s", verdict, lifecycle)
			return selfFact{outcome: o, at: at}, ok && c != "", nil
		})
}

const rollbackFactsSQL = `/* pg_sage */ SELECT l.id,
	COALESCE((SELECT o.action_class FROM sage.action_outcome o
	          WHERE o.action_log_id = l.id), ''),
	COALESCE((SELECT o.verdict FROM sage.action_outcome o WHERE o.action_log_id = l.id), ''),
	l.action_type, l.measured_at, COALESCE(d.reason, ''),
	COALESCE(d.evidence ? 'approved_by', false)
	FROM sage.action_log l
	LEFT JOIN sage.decision d ON d.id = l.decision_id
	WHERE l.outcome = 'rolled_back'
	  AND l.measured_at >= COALESCE($1::timestamptz,
	                                now() - make_interval(secs => $2::double precision))
	  AND NOT COALESCE(d.evidence ? 'incident_family', false)
	ORDER BY l.measured_at, l.id LIMIT $3`

// rollbackFacts are self-initiated actions rolled back for another reason
// than their own regression or the no-gain revert of an index create.
func (r *Reconciler) rollbackFacts(ctx context.Context, cursor *time.Time) ([]selfFact,
	*time.Time, error) {
	return r.readFacts(ctx, "read rolled-back actions", rollbackFactsSQL, cursor,
		func(rows pgx.Rows) (selfFact, bool, error) {
			var id int64
			var outcomeClass, verdict, label, reason string
			var approved bool
			var at time.Time
			if err := rows.Scan(&id, &outcomeClass, &verdict, &label, &at, &reason,
				&approved); err != nil {
				return selfFact{}, false, err
			}
			c := ClassForOutcomeClass(outcomeClass)
			if c == "" {
				c = classForActionLabel(label)
			}
			o := r.selfOutcome(id, c, actionLevel(reason, approved), at)
			o.Result, o.Source, o.Verdict = ResultRejected, SourceRollback, CauseRolledBack
			o.Detail = fmt.Sprintf("rolled back; verdict %q", verdict)
			return selfFact{outcome: o, at: at}, c != "" && rollbackIsDemerit(c, verdict), nil
		})
}

const rejectionFactsSQL = `/* pg_sage */ SELECT q.id, COALESCE(q.action_type, ''),
	q.proposed_sql, COALESCE(q.identity_key, ''), q.decided_at, q.decided_by
	FROM sage.action_queue q
	WHERE q.status = 'rejected' AND q.decided_by IS NOT NULL
	  AND q.decided_at >= COALESCE($1::timestamptz,
	                               now() - make_interval(secs => $2::double precision))
	ORDER BY q.decided_at, q.id LIMIT $3`

// rejectionFacts are proposals an operator rejected: a handoff of an
// incident pair, or a self-initiated class's approval item.
func (r *Reconciler) rejectionFacts(ctx context.Context, cursor *time.Time) ([]selfFact,
	*time.Time, error) {
	return r.readFacts(ctx, "read rejected proposals", rejectionFactsSQL, cursor,
		func(rows pgx.Rows) (selfFact, bool, error) {
			var id int64
			var actionType, sql, identity string
			var at time.Time
			var by int64
			if err := rows.Scan(&id, &actionType, &sql, &identity, &at, &by); err != nil {
				return selfFact{}, false, err
			}
			f, c := PairForQueued(actionType, sql, identity)
			ts := at.UTC()
			o := Outcome{Database: r.database, QueueID: id, Family: f, Class: c, Level: L2,
				Result: ResultRejected, Source: SourceOperator, Verdict: CauseRejected,
				Actor: fmt.Sprintf("user:%d", by), ObservedAt: &ts,
				Detail: "approval item rejected by an operator"}
			return selfFact{outcome: o, at: at}, f != "", nil
		})
}

// PairForQueued is the ledger pair of an approval item: the pair of its
// autonomy handoff key, else the trust family of its self-initiated
// class; family "" when the ledger does not judge it.
func PairForQueued(actionType, sql, identity string) (Family, ActionClass) {
	if strings.HasPrefix(identity, HandoffKeyPrefix) {
		if f, c, ok := parseHandoffKey(identity); ok {
			return f, c
		}
		return "", ""
	}
	if actionType == "" {
		return "", ""
	}
	c := ClassFor(policy.ActionRequest{SQL: sql,
		Contract: &policy.ActionContract{ActionType: actionType}})
	return SelfFamilyFor(c), c
}

// readFacts runs one kind's query from cursor and scans every row; keep
// reports a row the ledger records. It returns the newest time read.
func (r *Reconciler) readFacts(ctx context.Context, what, query string, cursor *time.Time,
	scan func(pgx.Rows) (selfFact, bool, error)) ([]selfFact, *time.Time, error) {
	rows, err := r.pool.Query(ctx, query, cursor, r.lookback.Seconds(), selfBatch)
	if err != nil {
		return nil, nil, fmt.Errorf("%s on %s: %w", what, r.database, err)
	}
	defer rows.Close()
	var out []selfFact
	var last *time.Time
	for rows.Next() {
		f, keep, err := scan(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("%s on %s: %w", what, r.database, err)
		}
		at := f.at.UTC()
		last = &at
		if keep {
			out = append(out, f)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("%s on %s: %w", what, r.database, err)
	}
	return out, last, nil
}

func (r *Reconciler) selfOutcome(actionLogID int64, c ActionClass, level Level,
	at time.Time) Outcome {
	ts := at.UTC()
	return Outcome{Database: r.database, ActionLogID: actionLogID, Family: SelfFamilyFor(c),
		Class: c, Level: level, Actor: ActorPgSage, ObservedAt: &ts}
}

// actionLevel is the level an action ran at: L3 when the ledger let it
// run unattended, L2 when an operator approved it, else L1.
func actionLevel(reason string, approved bool) Level {
	switch {
	case reason == string(policy.ReasonAutonomyL3):
		return L3
	case approved:
		return L2
	}
	return L1
}

// selfResult is the ledger result of a decided verdict of family: only
// an improvement is credit for a tuning class; for a hygiene class a
// neutral verdict that held (the action was not rolled back) is credit
// too; a regression is harmful; anything else earns nothing.
func selfResult(f Family, verdict, lifecycle string) (string, bool) {
	switch verdict {
	case verify.OutcomeImproved:
		return ResultVerifiedRecovery, true
	case verify.OutcomeRegressed:
		return ResultHarmful, true
	case verify.OutcomeNeutral:
		held := lifecycle != "rolled_back" && lifecycle != "rollback_failed"
		if f == FamilyHygiene && held {
			return ResultVerifiedRecovery, true
		}
		return ResultUnverified, true
	case verify.OutcomeInsufficient, verify.OutcomeUnverifiable:
		return ResultUnverified, true
	}
	return "", false
}

// rollbackIsDemerit reports a rollback that is an operator's verdict on
// the action: not the rollback of its own regression (already a
// demerit) nor the Phase 1.3 no-gain revert of a neutral index create.
func rollbackIsDemerit(c ActionClass, verdict string) bool {
	switch {
	case verdict == verify.OutcomeRegressed:
		return false
	case verdict == verify.OutcomeNeutral && c == ClassIndexCreate:
		return false
	}
	return true
}
