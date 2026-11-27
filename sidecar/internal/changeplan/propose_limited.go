package changeplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Plans proposed over MCP (plan-03b). An agent proposes; it never
// approves. A proposal is validated like every plan (plan-01a: each step
// cites a finding or evidence id that resolves) and stored proposed, and a
// principal may hold at most a limit of open plans, so one agent cannot
// flood the people who review them.

// DefaultOpenPlansPerPrincipal is the open-plan limit of one principal,
// like the pending-action limit of an agent principal.
const DefaultOpenPlansPerPrincipal = 5

// ErrOpenPlanLimit refuses a proposal from a principal at its limit.
var ErrOpenPlanLimit = errors.New("changeplan: the principal has too many open plans")

// OpenPlanLimitError is ErrOpenPlanLimit with the counts.
type OpenPlanLimitError struct {
	Open  int
	Limit int
}

func (e *OpenPlanLimitError) Error() string {
	return fmt.Sprintf("%v: %d open of %d; wait until a person decides one or reject one",
		ErrOpenPlanLimit, e.Open, e.Limit)
}

func (e *OpenPlanLimitError) Unwrap() error { return ErrOpenPlanLimit }

// openPlansSQL counts a proposer's plans that are not terminal.
const openPlansSQL = `/* pg_sage */ SELECT count(*) FROM sage.plan
	WHERE origin = $1 AND origin_ref = $2
	  AND status IN ('draft', 'proposed', 'approved', 'running', 'halted')`

// ProposeLimited stores p as a proposed plan when its proposer (p.Origin
// and p.OriginRef, which must be set) holds fewer than limit open plans.
// Proposals of one proposer serialize on an advisory lock, so concurrent
// ones never pass the limit together.
func (s *Store) ProposeLimited(ctx context.Context, p Plan, r Resolver,
	limit int) (Plan, error) {
	if limit < 1 {
		return Plan{}, fmt.Errorf("changeplan: open-plan limit %d is below 1", limit)
	}
	if strings.TrimSpace(p.OriginRef) == "" {
		return Plan{}, fmt.Errorf("%w: a limited proposal names its proposer (origin_ref)",
			ErrInvalid)
	}
	p.Status = StatusProposed
	return s.create(ctx, p, r, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(
			hashtextextended('pg_sage.plan.proposer:' || $1 || ':' || $2, 0))`,
			string(p.Origin), p.OriginRef); err != nil {
			return fmt.Errorf("lock the proposer's plans: %w", err)
		}
		var open int
		if err := tx.QueryRow(ctx, openPlansSQL, string(p.Origin),
			p.OriginRef).Scan(&open); err != nil {
			return fmt.Errorf("count the proposer's open plans: %w", err)
		}
		if open >= limit {
			return &OpenPlanLimitError{Open: open, Limit: limit}
		}
		return nil
	})
}

// ProposalNotifier hears every plan stored as proposed on a pool (the chat
// plan cards, plan-03b). It runs after the commit, with no lock held.
type ProposalNotifier func(ctx context.Context, p Plan)

var proposalNotifiers sync.Map // *pgxpool.Pool -> *notifierSlot

type notifierSlot struct{ fn ProposalNotifier }

// SetProposalNotifier makes fn hear the plans proposed on pool until the
// returned unset runs; a later registration replaces it, and an unset of a
// replaced registration does nothing.
func SetProposalNotifier(pool *pgxpool.Pool, fn ProposalNotifier) (unset func()) {
	if pool == nil || fn == nil {
		return func() {}
	}
	slot := &notifierSlot{fn: fn}
	proposalNotifiers.Store(pool, slot)
	return func() { proposalNotifiers.CompareAndDelete(pool, slot) }
}

// notifyProposed tells the pool's notifier, if any, about proposed plan p.
func (s *Store) notifyProposed(ctx context.Context, p Plan) {
	if p.Status != StatusProposed {
		return
	}
	if slot, ok := proposalNotifiers.Load(s.pool); ok {
		slot.(*notifierSlot).fn(ctx, p)
	}
}
