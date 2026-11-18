package api

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/gitops"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// ApprovalPRQueue is gitops.ApprovalQueue over the monitored databases
// (cli-02b): approval PRs read the same cards as the inbox and approve
// through approveAndRunExpecting, the browser's and chat's single-use,
// SQL-matched approval and the executor's run, so a merge adds no write
// path to a monitored database.
type ApprovalPRQueue struct {
	Mgr *fleet.DatabaseManager
}

var _ gitops.ApprovalQueue = (*ApprovalPRQueue)(nil)

func (q *ApprovalPRQueue) instance(database string) *fleet.DatabaseInstance {
	if q == nil || q.Mgr == nil {
		return nil
	}
	inst := q.Mgr.GetInstance(database)
	if inst == nil || inst.Pool == nil {
		return nil
	}
	return inst
}

// Pending implements gitops.ApprovalQueue, database by database in name
// order.
func (q *ApprovalPRQueue) Pending(ctx context.Context) ([]gitops.ApprovalItem, error) {
	if q == nil || q.Mgr == nil {
		return nil, nil
	}
	names := make([]string, 0)
	for name := range q.Mgr.Instances() {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []gitops.ApprovalItem
	for _, name := range names {
		inst := q.instance(name)
		if inst == nil {
			continue
		}
		list, err := store.NewActionStore(inst.Pool).ListPending(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("listing pending changes on %s: %w", name, err)
		}
		for _, a := range list {
			item, err := q.item(ctx, inst, a)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
	}
	return out, nil
}

// item is a queue row as its approval card and capped tier.
func (q *ApprovalPRQueue) item(ctx context.Context, inst *fleet.DatabaseInstance,
	a store.QueuedAction) (gitops.ApprovalItem, error) {
	l := approvalcard.Loader{Pool: inst.Pool, Database: inst.Name}
	if inst.Executor != nil {
		l.TrustLevel = inst.Executor.TrustLevel()
		l.Waits = inst.Executor.VerificationWaits()
	}
	c, err := l.Card(ctx, a.ID)
	if errors.Is(err, approvalcard.ErrNotFound) {
		return gitops.ApprovalItem{}, gitops.ErrItemNotFound
	}
	if err != nil {
		return gitops.ApprovalItem{}, fmt.Errorf("approval card %s/%d: %w", inst.Name, a.ID,
			err)
	}
	return gitops.ApprovalItem{Card: c, Tier: cappedTier(a, c)}, nil
}

// cappedTier is the higher of the executor contracts' tier and the
// card's; an unknown card tier is high (fail closed).
func cappedTier(a store.QueuedAction, c approvalcard.Card) policy.RiskTier {
	ranks := map[policy.RiskTier]int{policy.RiskSafe: 1, policy.RiskModerate: 2,
		policy.RiskHigh: 3}
	tier := approvalcard.ActionTier(a)
	card := policy.RiskTier(c.Risk.Tier)
	if c.Risk.Tier == "" {
		return tier
	}
	if _, ok := ranks[card]; !ok {
		return policy.RiskHigh
	}
	if ranks[card] > ranks[tier] {
		return card
	}
	return tier
}

// Item implements gitops.ApprovalQueue.
func (q *ApprovalPRQueue) Item(ctx context.Context, database string, queueID int,
) (gitops.ApprovalItem, error) {
	inst := q.instance(database)
	if inst == nil {
		// Not ErrItemNotFound: a database missing from the fleet for a moment
		// must not close its PRs; the next poll asks again.
		return gitops.ApprovalItem{}, fmt.Errorf("database %s is not monitored", database)
	}
	a, err := store.NewActionStore(inst.Pool).GetByID(ctx, queueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gitops.ApprovalItem{}, gitops.ErrItemNotFound
	}
	if err != nil {
		return gitops.ApprovalItem{}, err
	}
	return q.item(ctx, inst, *a)
}
