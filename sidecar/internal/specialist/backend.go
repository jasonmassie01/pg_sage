package specialist

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Backend is one database's investigator and action surfaces as the
// contract uses them. Nothing here bypasses pg_sage's own paths: a
// requested cancel goes through the action service's request (approval
// queue), a requested custodian action through the executor's pipeline.
type Backend interface {
	Start(ctx context.Context, t sre.Trigger) (sre.Investigation, bool, error)
	Detail(ctx context.Context, id sre.UUID) (sre.Detail, error)
	List(ctx context.Context, f sre.ListFilter) (sre.Page, error)
	// Proposals lists the investigation's cancel proposals; ErrNoActions
	// when the database has no action service.
	Proposals(ctx context.Context, id sre.UUID) ([]sreaction.ProposalView, error)
	RequestProposal(ctx context.Context, id sre.UUID, actor string) (sreaction.ProposalView,
		error)
	SubmitCustodian(ctx context.Context, inv sre.Investigation, p sre.ActionProposal,
		actor string) (GateOutcome, error)
	// ResolveQueryHash returns the queryids whose normalized text hashes to
	// hash (pg_stat_statements of this database).
	ResolveQueryHash(ctx context.Context, hash string) ([]int64, error)
	// Transcript is the redacted investigator transcript; sre.ErrNoTranscript
	// when the investigator never ran.
	Transcript(ctx context.Context, id sre.UUID, keepIdentifiers bool) (sre.TranscriptView,
		error)
}

// GateOutcome is what the gate decided for a submitted custodian proposal.
type GateOutcome struct {
	Decision string // executor policy decision
	Reason   string
	Detail   string
	ActionID int64
	Executed bool
	// HandedOff: queued for approval by the autonomy handoff.
	HandedOff bool
	// Stale: the custodian no longer proposes this action; nothing ran.
	Stale bool
}

// Directory resolves a database name to its backend.
type Directory interface {
	Backend(database string) (Backend, bool)
}

// CustodianSubmitter submits a custodian proposal a caller requested
// through the executor's pipeline (implemented by the sidecar wiring).
type CustodianSubmitter interface {
	SubmitCustodian(ctx context.Context, database string, inv sre.Investigation,
		p sre.ActionProposal, actor string) (GateOutcome, error)
}

// FleetDirectory serves backends from the fleet's database instances.
type FleetDirectory struct {
	mgr        *fleet.DatabaseManager
	custodians CustodianSubmitter
}

// NewFleetDirectory resolves names through mgr; custodians may be nil
// (custodian requests are then unavailable).
func NewFleetDirectory(mgr *fleet.DatabaseManager, custodians CustodianSubmitter) FleetDirectory {
	return FleetDirectory{mgr: mgr, custodians: custodians}
}

// Backend implements Directory.
func (d FleetDirectory) Backend(database string) (Backend, bool) {
	if d.mgr == nil {
		return nil, false
	}
	inst := d.mgr.GetInstance(database)
	if inst == nil {
		return nil, false
	}
	return fleetBackend{name: database, svc: inst.Investigations, actions: inst.Actions,
		custodians: d.custodians, pool: inst.Pool}, true
}

type fleetBackend struct {
	name       string
	svc        *sre.Service
	actions    *sreaction.ActionService
	custodians CustodianSubmitter
	pool       *pgxpool.Pool // the monitored database (query_hash resolution)
}

var errNoInvestigator = fmt.Errorf("%w: investigations are unavailable for this database",
	ErrUnavailable)

func (b fleetBackend) Start(ctx context.Context, t sre.Trigger) (sre.Investigation, bool,
	error) {
	if b.svc == nil {
		return sre.Investigation{}, false, errNoInvestigator
	}
	return b.svc.Start(ctx, t)
}

func (b fleetBackend) Detail(ctx context.Context, id sre.UUID) (sre.Detail, error) {
	if b.svc == nil {
		return sre.Detail{}, errNoInvestigator
	}
	return b.svc.Detail(ctx, id)
}

func (b fleetBackend) List(ctx context.Context, f sre.ListFilter) (sre.Page, error) {
	if b.svc == nil {
		return sre.Page{}, errNoInvestigator
	}
	return b.svc.List(ctx, f)
}

func (b fleetBackend) Proposals(ctx context.Context, id sre.UUID) ([]sreaction.ProposalView,
	error) {
	if b.actions == nil {
		return nil, ErrNoActions
	}
	return b.actions.Views(ctx, id)
}

func (b fleetBackend) RequestProposal(ctx context.Context, id sre.UUID,
	actor string) (sreaction.ProposalView, error) {
	if b.actions == nil {
		return sreaction.ProposalView{}, ErrNoActions
	}
	p, err := b.actions.RequestExecution(ctx, id, actor)
	if err != nil {
		return sreaction.ProposalView{}, err
	}
	return b.actions.View(ctx, p)
}

func (b fleetBackend) SubmitCustodian(ctx context.Context, inv sre.Investigation,
	p sre.ActionProposal, actor string) (GateOutcome, error) {
	if b.custodians == nil {
		return GateOutcome{}, fmt.Errorf("%w: custodian actions are not wired",
			ErrUnavailable)
	}
	return b.custodians.SubmitCustodian(ctx, b.name, inv, p, actor)
}

func (b fleetBackend) ResolveQueryHash(ctx context.Context, hash string) ([]int64, error) {
	return ResolveQueryHash(ctx, b.pool, hash)
}

func (b fleetBackend) Transcript(ctx context.Context, id sre.UUID,
	keepIdentifiers bool) (sre.TranscriptView, error) {
	if b.svc == nil {
		return sre.TranscriptView{}, errNoInvestigator
	}
	return b.svc.Transcript(ctx, id, sre.TranscriptOptions{KeepIdentifiers: keepIdentifiers})
}
