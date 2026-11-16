package changeplan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps plans in the monitored database's sage schema. Every write
// that changes what an approval binds (a step, the edges) locks the plan
// row (SELECT ... FOR UPDATE), voids a live approval in the same
// transaction and sets the plan back to proposed; Approve locks the same
// row and recomputes the hash from the rows, so an approval is never
// recorded on a hash the rows no longer have.
type Store struct {
	pool *pgxpool.Pool
	caps CapsSource
}

// NewStore returns a store on pool (the monitored database) that checks
// plans against the default plan caps.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, caps: noPolicyCaps} }

// WithCaps is s checking plans against the caps src reads (the standing
// policy's plans block) when they are proposed and approved.
func (s *Store) WithCaps(src CapsSource) *Store {
	if src == nil {
		src = noPolicyCaps
	}
	return &Store{pool: s.pool, caps: src}
}

// Errors the store returns; each is wrapped with the plan it concerns.
var (
	ErrNotFound      = errors.New("changeplan: no such plan or step")
	ErrInvalid       = errors.New("changeplan: the plan is invalid")
	ErrNotEditable   = errors.New("changeplan: the plan cannot be changed in its status")
	ErrNotApprovable = errors.New("changeplan: the plan cannot be approved in its status")
	ErrStaleHash     = errors.New("changeplan: the plan changed after it was shown; " +
		"review it again")
	ErrNotApproved  = errors.New("changeplan: the plan has no live approval")
	ErrHashMismatch = errors.New("changeplan: hash_mismatch: the plan's rows no longer " +
		"match the approved hash")
)

// VoidEdited is the void reason of an approval an edit voided.
const VoidEdited = "edited"

// maxListLimit bounds List.
const maxListLimit = 500

// InvalidPlanError carries the problems that refused a plan.
type InvalidPlanError struct {
	Problems []Problem
}

func (e *InvalidPlanError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Code+": "+p.Detail)
	}
	return ErrInvalid.Error() + ": " + strings.Join(parts, "; ")
}

func (e *InvalidPlanError) Unwrap() error { return ErrInvalid }

// Approval is a sage.plan_approval row: who approved which hash, in which
// mode (UpTo set for up_to) and where (Via is empty on rows stored before
// plan-03a).
type Approval struct {
	ID             int64      `json:"id"`
	PlanID         int64      `json:"plan_id"`
	PlanHash       string     `json:"plan_hash"`
	ApproverUserID int        `json:"approver_user_id"`
	Mode           Mode       `json:"mode"`
	UpTo           *int       `json:"up_to,omitempty"`
	Via            Via        `json:"via,omitempty"`
	DecidedAt      time.Time  `json:"decided_at"`
	VoidedAt       *time.Time `json:"voided_at,omitempty"`
	VoidReason     string     `json:"void_reason,omitempty"`
}

// ListFilter selects plans: by status ("" is every status), newest first,
// at most Limit (1..500).
type ListFilter struct {
	Status Status
	Limit  int
}

// querier is what reads need: a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// checkResult turns Validate's answer into the store's error.
func checkResult(problems []Problem, err error) error {
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return &InvalidPlanError{Problems: problems}
	}
	return nil
}

// Create validates p and stores it with its steps and edges as a draft
// or proposed plan (proposed when p.Status is empty), with its created
// event (plan-01b) in the same transaction. The plan returned is the
// stored one: ids, times, defaults and its plan hash.
func (s *Store) Create(ctx context.Context, p Plan, r Resolver) (Plan, error) {
	return s.create(ctx, p, r, nil)
}

// create is Create with guard run first in the transaction (nil: none); a
// stored proposed plan is told to the pool's proposal notifier.
func (s *Store) create(ctx context.Context, p Plan, r Resolver,
	guard func(context.Context, pgx.Tx) error) (Plan, error) {
	if p.Status == "" {
		p.Status = StatusProposed
	}
	if p.Status != StatusDraft && p.Status != StatusProposed {
		return Plan{}, fmt.Errorf("%w: a new plan is draft or proposed, not %s", ErrInvalid,
			p.Status)
	}
	p.Steps = slices.Clone(p.Steps) // the caller's steps are not changed
	for i := range p.Steps {
		p.Steps[i] = p.Steps[i].WithDefaults()
		p.Steps[i].Runtime = StepRuntime{State: StatePending}
	}
	if err := checkResult(Validate(ctx, p, r)); err != nil {
		return Plan{}, err
	}
	p.PlanHash = PlanHash(p)
	var id int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		if p.Status == StatusProposed {
			if err := checkCapsIn(ctx, tx, s.capsSource(), p); err != nil {
				return err
			}
		}
		var err error
		if id, err = insertPlan(ctx, tx, p); err != nil {
			return err
		}
		_, err = AppendEvent(ctx, tx, Event{PlanID: id, Kind: EventCreated,
			PlanHash: p.PlanHash, Actor: p.CreatedBy,
			Detail: map[string]any{"status": string(p.Status), "steps": len(p.Steps)}})
		return err
	})
	if err != nil {
		return Plan{}, fmt.Errorf("changeplan: create plan %q: %w", p.Goal, err)
	}
	stored, err := s.Get(ctx, id)
	if err == nil {
		s.notifyProposed(ctx, stored)
	}
	return stored, err
}

// Get returns plan id with its steps and edges (ErrNotFound when absent).
func (s *Store) Get(ctx context.Context, id int64) (Plan, error) {
	p, err := loadPlan(ctx, s.pool, id)
	if err != nil {
		return Plan{}, fmt.Errorf("changeplan: read plan %d: %w", id, err)
	}
	return p, nil
}

// List returns plans without their steps and edges, newest first.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Plan, error) {
	if f.Limit < 1 || f.Limit > maxListLimit {
		return nil, fmt.Errorf("changeplan: list limit %d is outside 1..%d", f.Limit,
			maxListLimit)
	}
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT `+planColumns+`
		  FROM sage.plan WHERE ($1 = '' OR status = $1) ORDER BY id DESC LIMIT $2`,
		string(f.Status), f.Limit)
	if err != nil {
		return nil, fmt.Errorf("changeplan: list plans: %w", err)
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, fmt.Errorf("changeplan: list plans: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("changeplan: list plans: %w", err)
	}
	return out, nil
}

// Approvals returns every approval of plan id, oldest first.
func (s *Store) Approvals(ctx context.Context, id int64) ([]Approval, error) {
	out, err := approvalsOf(ctx, s.pool, id)
	if err != nil {
		return nil, fmt.Errorf("changeplan: approvals of plan %d: %w", id, err)
	}
	return out, nil
}

// approvalColumns are the columns scanApproval reads.
const approvalColumns = `id, plan_id, plan_hash, approver_user_id, mode, up_to_ordinal,
	COALESCE(via, ''), decided_at, voided_at, COALESCE(void_reason, '')`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	err := row.Scan(&a.ID, &a.PlanID, &a.PlanHash, &a.ApproverUserID, &a.Mode, &a.UpTo,
		&a.Via, &a.DecidedAt, &a.VoidedAt, &a.VoidReason)
	return a, err
}

// approvalsOf reads every approval of plan id, oldest first.
func approvalsOf(ctx context.Context, q querier, id int64) ([]Approval, error) {
	rows, err := q.Query(ctx, `/* pg_sage */ SELECT `+approvalColumns+`
		  FROM sage.plan_approval WHERE plan_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approval, error) {
		return scanApproval(r)
	})
}

// inTx runs fn in one transaction bounded by lock and statement timeouts.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '30s'`); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
