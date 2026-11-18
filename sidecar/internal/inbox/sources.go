package inbox

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard/grants"
	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// Each source is a thin adapter over the store its own page already reads,
// on the monitored database's pool (where pg_sage keeps its tables).

// Instances finds a monitored database (*fleet.DatabaseManager or a
// read view of it).
type Instances interface {
	GetInstance(name string) *fleet.DatabaseInstance
}

// instanceOf is database's connected instance; ErrNotConnected without a
// pool, so the inbox counts it as unread rather than empty.
func instanceOf(in Instances, database string) (*fleet.DatabaseInstance, error) {
	var inst *fleet.DatabaseInstance
	if in != nil {
		inst = in.GetInstance(database)
	}
	if inst == nil || inst.Pool == nil {
		return nil, ErrNotConnected
	}
	return inst, nil
}

func poolOf(in Instances, database string) (*pgxpool.Pool, error) {
	inst, err := instanceOf(in, database)
	if err != nil {
		return nil, err
	}
	return inst.Pool, nil
}

// ChangeApprovals lists the action queue's pending, unexpired proposals.
func ChangeApprovals(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		pool, err := poolOf(in, db)
		if err != nil {
			return nil, err
		}
		pending, err := store.NewActionStore(pool).ListPending(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(pending))
		for _, a := range pending {
			id, due := strconv.Itoa(a.ID), a.ExpiresAt
			out = append(out, Item{Kind: KindChangeApproval, ID: id, Database: db,
				Title: "Approve " + nonEmpty(a.ActionType, "a change"), Why: a.Reason,
				Risk: a.ActionRisk, CreatedAt: a.ProposedAt, DueAt: &due,
				Href:   href("actions", "tab", "pending", "queue", id, "database", db),
				Source: "sage.action_queue", Class: a.ActionType})
		}
		return out, nil
	})
}

// Plans lists plans proposed or halted and steps waiting on a person.
func Plans(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		pool, err := poolOf(in, db)
		if err != nil {
			return nil, err
		}
		rows, err := changeplan.NewStore(pool).ListNeedingPerson(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(rows))
		for _, n := range rows {
			out = append(out, planItem(db, n))
		}
		return out, nil
	})
}

func planItem(db string, n changeplan.NeedingPerson) Item {
	plan := strconv.FormatInt(n.PlanID, 10)
	it := Item{Kind: KindPlanApproval, ID: plan, Database: db, Title: n.Goal,
		Risk: RiskModerate, CreatedAt: n.Since, Source: "sage.plan",
		Href: href("actions", "tab", "plans", "plan", plan, "database", db)}
	switch {
	case n.StepOrdinal > 0:
		it.ID = fmt.Sprintf("%s/%d", plan, n.StepOrdinal)
		it.Why = fmt.Sprintf("step %d is %s", n.StepOrdinal, n.StepState)
		if n.Reason != "" {
			it.Why += ": " + n.Reason
		}
		it.Source = "sage.plan_step"
	case n.Status == changeplan.StatusHalted:
		it.Why, it.Risk = "the plan halted mid-change; resume, revert or cancel it", RiskHigh
	default:
		it.Why = "the plan waits for approval"
	}
	return it
}

// Facts lists facts proposed and not yet confirmed or rejected.
func Facts(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		pool, err := poolOf(in, db)
		if err != nil {
			return nil, err
		}
		got, err := facts.NewStore(pool).List(ctx, facts.Filter{
			Status: []facts.Status{facts.StatusProposed}, Limit: 200})
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(got))
		for _, f := range got {
			id := strconv.FormatInt(f.ID, 10)
			out = append(out, Item{Kind: KindFact, ID: id, Database: db,
				Title: f.Describe(), Why: nonEmpty(f.Rationale, "proposed by "+f.ProposedBy),
				Risk: RiskSafe, CreatedAt: f.CreatedAt, Source: "sage.facts",
				Href: href("facts", "database", db, "fact", id)})
		}
		return out, nil
	})
}

// AgentRequests lists agents' unexpired pending capability requests.
func AgentRequests(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		pool, err := poolOf(in, db)
		if err != nil {
			return nil, err
		}
		got, err := grants.ListPendingRequests(ctx, pool, grants.MaxPendingRequests)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(got))
		for _, r := range got {
			id, due := strconv.FormatInt(r.ID, 10), r.ExpiresAt
			out = append(out, Item{Kind: KindAgentRequest, ID: id, Database: db,
				Title: fmt.Sprintf("Agent %s asks for %s for %d min", r.PrincipalID,
					r.Capability, r.DurationMinutes),
				Why: r.Reason, Risk: RiskHigh, CreatedAt: r.RequestedAt, DueAt: &due,
				Href: href("agents", "agent", r.PrincipalID, "request", id,
					"database", db),
				Source: "sage.guard_grant_requests"})
		}
		return out, nil
	})
}

// PolicyProposals lists policy proposals waiting for ratification (an
// agent's propose_policy_change among them); fleetpol-02 exceptions join
// this kind through the same interface.
func PolicyProposals(in Instances) Source {
	return SourceFunc(func(ctx context.Context, db string) ([]Item, error) {
		pool, err := poolOf(in, db)
		if err != nil {
			return nil, err
		}
		got, err := policy.NewStore(pool).OpenProposals(ctx, policy.MaxOpenProposals)
		if err != nil {
			return nil, err
		}
		out := make([]Item, 0, len(got))
		for _, p := range got {
			out = append(out, policyItem(db, p))
		}
		return out, nil
	})
}

func policyItem(db string, p policy.Proposal) Item {
	id := strconv.FormatInt(p.ID, 10)
	who := p.Actor
	if p.PrincipalID != "" {
		who = "agent " + p.PrincipalID
	}
	it := Item{Kind: KindPolicyException, ID: id, Database: db,
		Title: "Policy change (" + p.Profile + ") proposed by " + who,
		Why:   "a policy change takes effect only once an admin ratifies it",
		Risk:  RiskModerate, CreatedAt: p.CreatedAt, Source: "sage.policy",
		Href: href("agents", "tab", "policy", "proposal", id, "database", db)}
	if p.Widening {
		it.Why, it.Risk = "it widens what pg_sage or an agent may do; ratify or reject it",
			RiskHigh
	}
	return it
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
