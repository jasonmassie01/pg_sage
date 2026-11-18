package modelgate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/modellift"
	"github.com/pg-sage/sidecar/internal/review/plan"
	"github.com/pg-sage/sidecar/internal/review/scorecard"
)

// Errors of the gate.
var (
	ErrNoStore      = errors.New("model gate: no database to read or write")
	ErrStale        = errors.New("model gate: a newer G-M1 evaluation supersedes this one")
	ErrNotFound     = errors.New("model gate: no such gate evaluation")
	ErrInvalidInput = errors.New("model gate: invalid input")
)

// gateLock serializes every evaluation and approval of one sage schema.
const gateLock = `/* pg_sage */ SELECT pg_advisory_xact_lock(hashtext('sage.review_gate'))`

// Options are review.gate's thresholds and fixture databases, and whether
// the operator set review.model_pass on (nil: not on).
type Options struct {
	Thresholds Thresholds
	Fixtures   []string
	OperatorOn func() bool
}

// Gate evaluates and records G-M1 and G-M2 for one database's reviews, in
// that database's sage schema, where review-03b's authority flag lives.
type Gate struct {
	db       *pgxpool.Pool
	database string
	opts     Options
}

// New is the gate of database's reviews on db.
func New(db *pgxpool.Pool, database string, o Options) *Gate {
	return &Gate{db: db, database: database, opts: o}
}

// Result is one evaluation. Revoked names the models whose live grant the
// evaluation revoked because another model (or none) now runs.
type Result struct {
	Model    string   `json:"model"`
	Evidence Evidence `json:"evidence"`
	M1       Verdict  `json:"m1"`
	M1State  State    `json:"m1_state"`
	M2       Verdict  `json:"m2"`
	M2State  State    `json:"m2_state"`
	Revoked  []string `json:"revoked"`
}

// Evaluate judges running (the review model in use now; "" when none) at
// tick. Grants of other models are revoked first. An error leaves nothing
// granted and nothing written: the gate never defaults to eligible.
func (g *Gate) Evaluate(ctx context.Context, running string, tick time.Time) (Result, error) {
	if g == nil || g.db == nil {
		return Result{}, ErrNoStore
	}
	th := g.opts.Thresholds
	if !th.valid() {
		return Result{}, fmt.Errorf("%w: thresholds %+v", ErrInvalidInput, th)
	}
	model := modellift.NormalizeModel(running)
	res := Result{Model: model, Revoked: []string{}}
	if model != "" {
		sc, err := scorecard.NewStore(g.db).Read(ctx, g.database, time.Time{},
			g.opts.Fixtures)
		if err != nil {
			return Result{}, fmt.Errorf("model gate: read the scorecard of %s: %w",
				g.database, err)
		}
		res.Evidence = EvidenceFor(sc.PerReview, model, th)
		res.M1, res.M2 = EvaluateM1(res.Evidence, th), EvaluateM2(res.Evidence, th)
	}
	err := g.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if res.Revoked, err = retireOthers(ctx, tx, model, tick); err != nil || model == "" {
			return err
		}
		return g.apply(ctx, tx, tick, &res)
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

func (g *Gate) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := g.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("model gate: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, gateLock); err != nil {
		return fmt.Errorf("model gate: take the gate lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("model gate: commit: %w", err)
	}
	return nil
}

// apply records res's verdicts for res.Model, unless another evaluation of
// the same tick already did (its states are then reported).
func (g *Gate) apply(ctx context.Context, tx pgx.Tx, tick time.Time, res *Result) error {
	m1, err := latest(ctx, tx, res.Model, RuleM1)
	if err != nil {
		return err
	}
	if m1 != nil && m1.Tick.Equal(tick) {
		res.M1State = m1.Verdict
		m2, err := latest(ctx, tx, res.Model, RuleM2)
		if err == nil && m2 != nil {
			res.M2State = m2.Verdict
		}
		return err
	}
	rec := record{Evidence: res.Evidence, Verdict: res.M1, Thresholds: g.opts.Thresholds}
	if res.M1State, err = applyM1(ctx, tx, m1, tick, rec); err != nil {
		return err
	}
	rec.Verdict = res.M2
	res.M2State, err = g.applyM2(ctx, tx, res.Model, tick, rec)
	return err
}

// grantOf is model's live authority grant (who, when), or ok false.
func grantOf(ctx context.Context, db dbtx, model string) (string, time.Time, bool, error) {
	var by string
	var at time.Time
	err := db.QueryRow(ctx, `/* pg_sage */ SELECT granted_by, granted_at
		FROM sage.review_model_authority WHERE model = $1 AND revoked_at IS NULL`, model).
		Scan(&by, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("model gate: read the authority of %s: %w",
			model, err)
	}
	return by, at, true, nil
}

// applyM1 moves G-M1 for rec's model from its latest row prev.
func applyM1(ctx context.Context, tx pgx.Tx, prev *Row, tick time.Time,
	rec record) (State, error) {
	model := rec.Evidence.Model
	by, at, live, err := grantOf(ctx, tx, model)
	if err != nil {
		return "", err
	}
	cur := StateNotEligible
	if prev != nil {
		cur = prev.Verdict
	}
	switch {
	case live:
		// A live grant without an approved latest row is unbacked: it is
		// revoked like a failed window (fail closed).
		cur = StateGranted
		if prev == nil || prev.Verdict != StateGranted {
			rec.Verdict.Eligible = false
			rec.Verdict.Reason = "a live grant with no approved G-M1 evaluation behind it; " +
				rec.Verdict.Reason
		}
	case cur == StateGranted:
		cur = StateRevoked
	}
	event := EventFail
	if rec.Verdict.Eligible {
		event = EventPass
	}
	next, err := NextM1(cur, event)
	if err == nil && next == StateEligible {
		next, err = NextM1(next, EventFiled)
	}
	if err != nil {
		return "", err
	}
	row := newRow{model: model, rule: RuleM1, tick: tick, verdict: next,
		reason: summary(rec.Verdict), rec: rec}
	return writeM1(ctx, tx, cur, row, by, at)
}

// writeM1 records next (row.verdict) after cur: a grant keeps its approval,
// a revocation drops the flag, and a revoked model that still fails is
// simply not eligible.
func writeM1(ctx context.Context, tx pgx.Tx, cur State, row newRow, by string,
	at time.Time) (State, error) {
	switch {
	case row.verdict == StateGranted:
		row.approvedBy, row.approvedAt = by, &at
	case row.verdict == StateRevoked && cur == StateGranted:
		row.revoked = true
		if err := plan.RevokeModelAuthority(ctx, tx, row.model,
			clip(row.reason, 1000)); err != nil {
			return "", err
		}
	case row.verdict == StateRevoked:
		row.verdict = StateNotEligible
	}
	if _, err := insert(ctx, tx, row); err != nil {
		return "", err
	}
	return row.verdict, nil
}

func (g *Gate) applyM2(ctx context.Context, tx pgx.Tx, model string, tick time.Time,
	rec record) (State, error) {
	prev, err := latest(ctx, tx, model, RuleM2)
	if err != nil {
		return "", err
	}
	var cur State
	if prev != nil {
		cur = prev.Verdict
	}
	on := g.opts.OperatorOn != nil && g.opts.OperatorOn()
	next := NextM2(cur, rec.Verdict.Eligible, on)
	reason := summary(rec.Verdict)
	if on && rec.Verdict.Eligible {
		reason = "review.model_pass is on, so the operator keeps the model pass; " + reason
	}
	_, err = insert(ctx, tx, newRow{model: model, rule: RuleM2, tick: tick, verdict: next,
		reason: reason, rec: rec})
	return next, err
}

// retireOthers revokes every live grant of a model other than model, and
// withdraws another model's pending promotion, at tick.
func retireOthers(ctx context.Context, tx pgx.Tx, model string, tick time.Time) ([]string,
	error) {
	why := fmt.Sprintf("the running review model is now %q", model)
	if model == "" {
		why = "no review model is running"
	}
	others, err := queryRows(ctx, tx, "other models' latest G-M1 rows", `/* pg_sage */
		SELECT DISTINCT ON (model) `+rowColumns+` FROM sage.review_gate
		WHERE rule = 'G-M1' AND model <> $1 ORDER BY model, id DESC`, model)
	if err != nil {
		return nil, err
	}
	if others, err = withUnbackedGrants(ctx, tx, model, others); err != nil {
		return nil, err
	}
	revoked := []string{}
	for _, o := range others {
		_, _, live, err := grantOf(ctx, tx, o.Model)
		if err != nil {
			return nil, err
		}
		if !live && o.Verdict != StatePending {
			continue
		}
		cur := StatePending
		if live {
			cur, revoked = StateGranted, append(revoked, o.Model)
		}
		next, _ := NextM1(cur, EventModelChanged)
		row := newRow{model: o.Model, rule: RuleM1, tick: tick, verdict: next,
			reason: RuleM1 + ": " + why, rec: record{Evidence: Evidence{Model: o.Model}}}
		if _, err := writeM1(ctx, tx, cur, row, "", time.Time{}); err != nil {
			return nil, err
		}
	}
	return revoked, nil
}

// Approve is a person's approval of the live G-M1 promotion id: it becomes
// the granted row and sets review-03b's authority flag for its model, in
// one transaction. Only the newest G-M1 evaluation, still pending, can be
// approved.
func (g *Gate) Approve(ctx context.Context, id int64, by string) (Row, error) {
	if g == nil || g.db == nil {
		return Row{}, ErrNoStore
	}
	by = strings.TrimSpace(by)
	if by == "" {
		return Row{}, fmt.Errorf("%w: an approval names who approved it", ErrInvalidInput)
	}
	var out Row
	err := g.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = approve(ctx, tx, id, by)
		return err
	})
	return out, err
}

func approve(ctx context.Context, tx pgx.Tx, id int64, by string) (Row, error) {
	row, err := scanRow(tx.QueryRow(ctx, `/* pg_sage */ SELECT `+rowColumns+`
		FROM sage.review_gate WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Row{}, fmt.Errorf("%w: id %d", ErrNotFound, id)
	}
	if err != nil {
		return Row{}, fmt.Errorf("model gate: read gate row %d: %w", id, err)
	}
	if row.Rule != RuleM1 || row.Verdict != StatePending || row.ApprovedAt != nil {
		return Row{}, fmt.Errorf("%w: row %d is %s %s", ErrNotPending, id, row.Rule,
			row.Verdict)
	}
	var newest int64
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT max(id) FROM sage.review_gate
		WHERE rule = 'G-M1'`).Scan(&newest); err != nil {
		return Row{}, fmt.Errorf("model gate: read the newest G-M1 row: %w", err)
	}
	if newest != id {
		return Row{}, fmt.Errorf("%w: row %d, newest %d", ErrStale, id, newest)
	}
	row, err = scanRow(tx.QueryRow(ctx, `/* pg_sage */ UPDATE sage.review_gate
		SET verdict = 'granted', approved_by = $2, approved_at = now() WHERE id = $1
		RETURNING `+rowColumns, id, clip(by, 200)))
	if err != nil {
		return Row{}, fmt.Errorf("model gate: record the approval of row %d: %w", id, err)
	}
	if err := plan.GrantModelAuthority(ctx, tx, row.Model, by,
		clip(row.Reason, 1000)); err != nil {
		return Row{}, err
	}
	return row, nil
}

// withUnbackedGrants adds a row for every other model with a live grant but
// no G-M1 row, so a grant made outside the gate is revoked too.
func withUnbackedGrants(ctx context.Context, tx pgx.Tx, model string, others []Row) ([]Row,
	error) {
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT model FROM sage.review_model_authority
		a WHERE revoked_at IS NULL AND model <> $1 AND NOT EXISTS (SELECT 1
		FROM sage.review_gate g WHERE g.rule = 'G-M1' AND g.model = a.model)`, model)
	if err != nil {
		return nil, fmt.Errorf("model gate: read other models' grants: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("model gate: read other models' grants: %w", err)
	}
	for _, n := range names {
		others = append(others, Row{Model: n, Rule: RuleM1, Verdict: StateGranted})
	}
	return others, nil
}
