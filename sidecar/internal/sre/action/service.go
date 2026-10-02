package action

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/store"
)

// ActionDeps wires one database's action service.
type ActionDeps struct {
	// Service is the database's investigation service (scope, store,
	// durability).
	Service *sre.Service
	// Targets runs the action probes (probes.ActionRegistry) on the
	// monitored database.
	Targets sre.ProbeRunner
	// Queue is the monitored database's approval queue.
	Queue ApprovalQueue
	// Executor runs approved cancels (the database's executor).
	Executor BackendCanceller
	// Notifier sends approval requests to chat; nil sends none.
	Notifier ApprovalNotifier
	Config   ActionConfig
	LogFn    func(string, string, ...any)
	// Now is the scheduling clock (recovery samples, deadlines); nil is
	// time.Now.
	Now func() time.Time
}

// ActionService proposes, hands off and verifies one database's actions.
type ActionService struct {
	svc      *sre.Service
	st       *sre.PostgresStore
	ps       proposalStore
	targets  sre.ProbeRunner
	queue    ApprovalQueue
	exec     BackendCanceller
	notifier ApprovalNotifier
	cfg      ActionConfig
	logFn    func(string, string, ...any)
	now      func() time.Time
}

// systemActor is the actor of automatic proposals and checks.
const systemActor = "system:sre-actions"

// NewActionService validates the wiring.
func NewActionService(d ActionDeps) (*ActionService, error) {
	if d.Service == nil || d.Service.Coordinator() == nil || d.Service.Store() == nil ||
		d.Targets == nil || d.Queue == nil || d.Executor == nil {
		return nil, fmt.Errorf("%w: action service needs investigations, target "+
			"probes, an approval queue and an executor", sre.ErrInvalidRequest)
	}
	if err := d.Config.Validate(); err != nil {
		return nil, err
	}
	a := &ActionService{svc: d.Service, st: d.Service.Store(),
		ps: proposalStore{st: d.Service.Store()}, targets: d.Targets, queue: d.Queue,
		exec: d.Executor, notifier: d.Notifier, cfg: d.Config, logFn: d.LogFn, now: d.Now}
	if a.logFn == nil {
		a.logFn = func(string, string, ...any) {}
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

func (a *ActionService) scope(ctx context.Context) (sre.Scope, error) {
	return a.svc.Scope(ctx)
}

// observe records a store result in the durability guard.
func (a *ActionService) observe(err error) error {
	a.svc.Coordinator().Durability().Observe(err)
	return err
}

// handoffAllowed is false while metadata durability is degraded
// (CHECK-16): no proposal is handed to the approval flow or executor.
func (a *ActionService) handoffAllowed() bool {
	return a.svc.Coordinator().Durability().HandoffAllowed()
}

// Get reads one proposal of this database.
func (a *ActionService) Get(ctx context.Context, id sre.UUID) (Proposal, error) {
	scope, err := a.scope(ctx)
	if err != nil {
		return Proposal{}, err
	}
	p, err := a.ps.get(ctx, scope, id)
	return p, a.observe(err)
}

// ForInvestigation lists an investigation's proposals.
func (a *ActionService) ForInvestigation(ctx context.Context, invID sre.UUID) ([]Proposal,
	error) {
	scope, err := a.scope(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := a.st.Get(ctx, scope, invID); err != nil {
		return nil, a.observe(err)
	}
	ps, err := a.ps.list(ctx, scope, `investigation_id = $3 ORDER BY created_at`,
		string(invID))
	return ps, a.observe(err)
}

// Propose derives the investigation's evidence-matched cancel (or records
// why there is none). It is idempotent: an existing proposal is returned.
// Proposing never queues or executes anything.
func (a *ActionService) Propose(ctx context.Context, invID sre.UUID,
	actor string) (Proposal, error) {
	scope, err := a.scope(ctx)
	if err != nil {
		return Proposal{}, err
	}
	if _, err := sre.ParseUUID(string(invID)); err != nil {
		return Proposal{}, err
	}
	if existing, err := a.existing(ctx, scope, invID); err == nil ||
		!errors.Is(err, ErrProposalNotFound) {
		return existing, err
	}
	inv, err := a.st.Get(ctx, scope, invID)
	if err != nil {
		return Proposal{}, a.observe(err)
	}
	if inv.State.Live() {
		return Proposal{}, fmt.Errorf("%w: the investigation is still %s",
			ErrProposalState, inv.State)
	}
	p, err := a.derive(ctx, inv)
	if err != nil {
		return Proposal{}, err
	}
	out, _, err := a.ps.insert(ctx, p, actor, proposedEvent(p))
	return out, a.observe(err)
}

func (a *ActionService) existing(ctx context.Context, scope sre.Scope,
	invID sre.UUID) (Proposal, error) {
	ps, err := a.ps.list(ctx, scope, `investigation_id = $3 AND action_class = $4`,
		string(invID), string(ActionCancelBackend))
	if err != nil {
		return Proposal{}, a.observe(err)
	}
	if len(ps) == 0 {
		return Proposal{}, ErrProposalNotFound
	}
	return ps[0], nil
}

// derive builds the proposal from the investigation's latest diagnosis,
// its cited evidence and a fresh sample of the target.
func (a *ActionService) derive(ctx context.Context, inv sre.Investigation) (Proposal,
	error) {
	hyps, err := a.st.Hypotheses(ctx, inv.Scope, inv.ID)
	if err != nil {
		return Proposal{}, a.observe(err)
	}
	ev, err := a.st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil {
		return Proposal{}, a.observe(err)
	}
	p := Proposal{ID: sre.NewUUID(), Scope: inv.Scope, InvestigationID: inv.ID,
		Class: ActionCancelBackend, Family: inv.Summary.Family, Node: inv.Summary.Root,
		Contract:  executor.CancelBackendRepairContract(),
		ExpiresAt: a.now().Add(a.cfg.ApprovalTTL)}
	c, why := deriveCancel(inv, sre.LatestRevision(hyps), ev)
	var target BackendTarget
	if why == nil {
		p.Family, p.Node, p.EvidenceIDs, p.Baseline = c.Family, c.Node, c.EvidenceIDs,
			c.Baseline
		target, why = a.checkCandidate(ctx, c)
	}
	if why != nil {
		p.State, p.Reason, p.Detail = ProposalIneligible, why.Reason, why.Detail
		return p, nil
	}
	p.State, p.Target, p.SQL = ProposalProposed, &target, cancelSQL(target.PID)
	p.Policy = verdictOf(a.exec.PreviewBackendCancel(ctx, target.InRecovery))
	return p, nil
}

func proposedEvent(p Proposal) actionEvent {
	payload := map[string]any{"proposal_id": string(p.ID), "class": string(p.Class),
		"state": string(p.State)}
	if p.Reason != "" {
		payload["reason"], payload["detail"] = string(p.Reason), p.Detail
	}
	if p.Target != nil {
		payload["pid"], payload["backend_start"] = p.Target.PID, p.Target.BackendStart
		payload["evidence_ids"] = evidenceText(p.EvidenceIDs)
		payload["policy"] = p.Policy.Decision
	}
	return actionEvent{typ: "action_proposed", payload: payload}
}

// Owns reports whether an approval item is one of this database's
// proposals.
func (a *ActionService) Owns(action store.QueuedAction) bool {
	id, ok := ProposalIDFromIdentityKey(action.IdentityKey)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.Get(ctx, id)
	return err == nil
}

// Database is the service's database name.
func (a *ActionService) Database() string { return a.svc.Name() }
