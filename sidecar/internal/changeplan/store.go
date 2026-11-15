package changeplan

import (
	"context"
	"errors"
	"fmt"
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
}

// NewStore returns a store on pool (the monitored database).
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

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

// Approval is a sage.plan_approval row.
type Approval struct {
	ID             int64      `json:"id"`
	PlanID         int64      `json:"plan_id"`
	PlanHash       string     `json:"plan_hash"`
	ApproverUserID int        `json:"approver_user_id"`
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
// or proposed plan (proposed when p.Status is empty). The plan returned
// is the stored one: ids, times, defaults and its plan hash.
func (s *Store) Create(ctx context.Context, p Plan, r Resolver) (Plan, error) {
	if p.Status == "" {
		p.Status = StatusProposed
	}
	if p.Status != StatusDraft && p.Status != StatusProposed {
		return Plan{}, fmt.Errorf("%w: a new plan is draft or proposed, not %s", ErrInvalid,
			p.Status)
	}
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
		var err error
		id, err = insertPlan(ctx, tx, p)
		return err
	})
	if err != nil {
		return Plan{}, fmt.Errorf("changeplan: create plan %q: %w", p.Goal, err)
	}
	return s.Get(ctx, id)
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
	rows, err := s.pool.Query(ctx, `/* pg_sage */ SELECT id, plan_id, plan_hash,
		approver_user_id, decided_at, voided_at, COALESCE(void_reason, '')
		  FROM sage.plan_approval WHERE plan_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("changeplan: approvals of plan %d: %w", id, err)
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.ID, &a.PlanID, &a.PlanHash, &a.ApproverUserID, &a.DecidedAt,
			&a.VoidedAt, &a.VoidReason); err != nil {
			return nil, fmt.Errorf("changeplan: approvals of plan %d: %w", id, err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("changeplan: approvals of plan %d: %w", id, err)
	}
	return out, nil
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
