package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Ask Sage wiring (roadmap phase 3). Each database runtime registers its
// Ask Sage service in one process-wide registry that the API and MCP
// route through. Ask Sage reaches the existing paths only through these
// adapters: a proposal through executor.ProposeFindingForApproval (queue
// for a person, never execute), an investigation through the operator
// trigger of sre.Service.Start, and reads through the redacted
// investigation service and the Trust view.

var (
	askRegistryOnce sync.Once
	askRegistry     *ask.Registry
)

// askServices is the process-wide Ask Sage registry.
func askServices() *ask.Registry {
	askRegistryOnce.Do(func() { askRegistry = ask.NewRegistry() })
	return askRegistry
}

// startAsk registers the database's Ask Sage until the runtime stops.
func (rt *databaseRuntime) startAsk() {
	d := ask.Deps{Database: rt.spec.Name, Pool: rt.spec.Pool, Config: rt.cfg.Ask,
		Settings: rt.cfg, Queries: agenttools.New(rt.spec.Pool, agentToolOptions(rt.cfg)),
		Log: logStructuredWrapper}
	if registry := processAutonomy().registry; registry != nil {
		d.Trust = askTrust{viewer: registry, database: rt.spec.Name}
	}
	if rt.generalLLM != nil {
		d.Model = rt.generalLLM
	}
	if rt.sreService != nil {
		d.Investigations = askInvestigations{svc: rt.sreService}
		d.Starter = askStarter{svc: rt.sreService}
	}
	if rt.executor != nil {
		d.Proposer = askProposer{inner: rt.executor}
	}
	svc, err := ask.New(d)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: Ask Sage not started: %v", rt.spec.Name, err)
		return
	}
	askServices().Set(rt.spec.Name, svc)
	rt.start(func() {
		<-rt.ctx.Done()
		askServices().Remove(rt.spec.Name, svc)
	})
	rt.note("ask_sage")
}

// findingProposer is the executor's queue-never-execute proposal path.
type findingProposer interface {
	ProposeFindingForApproval(ctx context.Context, findingID int64,
		origin executor.ProposalOrigin) (executor.FindingProposal, error)
}

type askProposer struct{ inner findingProposer }

func (p askProposer) ProposeFinding(ctx context.Context, findingID int64,
	actor string) (ask.Proposal, error) {
	if p.inner == nil {
		return ask.Proposal{}, fmt.Errorf("%w: no executor", ask.ErrUnavailable)
	}
	fp, err := p.inner.ProposeFindingForApproval(ctx, findingID,
		executor.ProposalOrigin{Via: executor.ProposedViaAskSage, By: actor})
	switch {
	case errors.Is(err, executor.ErrNotProposable):
		return ask.Proposal{}, fmt.Errorf("%w: %v", ask.ErrRefused, err)
	case errors.Is(err, executor.ErrProposalBlocked):
		return ask.Proposal{}, fmt.Errorf("%w: %v", ask.ErrBlocked, err)
	case errors.Is(err, executor.ErrApprovalQueueUnavailable):
		return ask.Proposal{}, fmt.Errorf("%w: %v", ask.ErrUnavailable, err)
	case err != nil:
		return ask.Proposal{}, fmt.Errorf("propose finding %d for %s: %w", findingID, actor,
			err)
	}
	prediction, err := json.Marshal(fp.Prediction)
	if err != nil {
		return ask.Proposal{}, fmt.Errorf("encode the predicted effect: %w", err)
	}
	return ask.Proposal{QueueID: int64(fp.QueueID), FindingID: fp.FindingID,
		Created: fp.Created, Verdict: fp.Decision.Decision, Reason: fp.Decision.BlockedReason,
		RiskTier: fp.Decision.RiskTier, ActionType: fp.ActionType, SQL: fp.SQL,
		RollbackSQL: fp.RollbackSQL, RollbackClass: fp.RollbackClass,
		Prediction: prediction}, nil
}

// investigationService is what Ask Sage uses of sre.Service.
type investigationService interface {
	Start(ctx context.Context, t sre.Trigger) (sre.Investigation, bool, error)
	List(ctx context.Context, f sre.ListFilter) (sre.Page, error)
	Detail(ctx context.Context, id sre.UUID) (sre.Detail, error)
}

type askStarter struct{ svc investigationService }

func (s askStarter) StartInvestigation(ctx context.Context, r ask.StartRequest) (ask.Started,
	error) {
	if s.svc == nil {
		return ask.Started{}, fmt.Errorf("%w: no investigator", ask.ErrUnavailable)
	}
	inv, created, err := s.svc.Start(ctx, sre.Trigger{CaseID: r.CaseID,
		Kind: sre.TriggerOperator, Subject: r.Subject, IdempotencyKey: r.CaseID,
		Actor: r.Actor})
	if errors.Is(err, sre.ErrInvalidRequest) {
		return ask.Started{}, fmt.Errorf("%w: %v", ask.ErrRefused, err)
	}
	if err != nil {
		return ask.Started{}, fmt.Errorf("open investigation: %w", err)
	}
	return ask.Started{ID: string(inv.ID), Created: created}, nil
}

type askInvestigations struct{ svc investigationService }

func (a askInvestigations) ListInvestigations(ctx context.Context, limit int) (
	json.RawMessage, error) {
	page, err := a.svc.List(ctx, sre.ListFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	return json.Marshal(page.Items)
}

func (a askInvestigations) Investigation(ctx context.Context, id string) (json.RawMessage,
	error) {
	d, err := a.svc.Detail(ctx, sre.UUID(id))
	if errors.Is(err, sre.ErrNotFound) {
		return nil, fmt.Errorf("%w: investigation %s", ask.ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

// trustViewer is the earned-autonomy registry's Trust view.
type trustViewer interface {
	TrustView(ctx context.Context, database string) (earned.TrustView, bool, error)
}

type askTrust struct {
	viewer   trustViewer
	database string
}

func (a askTrust) Trust(ctx context.Context) (json.RawMessage, error) {
	if a.viewer == nil {
		return nil, fmt.Errorf("%w: no trust ledger", ask.ErrNotFound)
	}
	view, ok, err := a.viewer.TrustView(ctx, a.database)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: no trust ledger for %s", ask.ErrNotFound, a.database)
	}
	return json.Marshal(view)
}
