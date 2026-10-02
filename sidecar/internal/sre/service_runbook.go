package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Runbooks and incident memory on the service: what the API and MCP tools
// call, scoped to the database's bound identity. Role checks live at the
// surfaces; the store still refuses a signature by anyone but an admin.

// Runbooks lists the database's runbooks with their latest version.
func (s *Service) Runbooks(ctx context.Context) ([]Runbook, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.ListRunbooks(ctx, scope)
}

// Runbook returns one runbook with every version.
func (s *Service) Runbook(ctx context.Context, id UUID) (Runbook, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Runbook{}, err
	}
	return s.store.GetRunbook(ctx, scope, id)
}

// CreateRunbook stores a hand-written definition as an unsigned draft.
func (s *Service) CreateRunbook(ctx context.Context, def runbook.Definition,
	actor string) (Runbook, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Runbook{}, err
	}
	return s.store.CreateRunbook(ctx, scope, RunbookInput{Definition: def, Actor: actor})
}

// ReviseRunbook appends an unsigned draft version on top of base.
func (s *Service) ReviseRunbook(ctx context.Context, id UUID, base int,
	def runbook.Definition, actor string) (Runbook, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Runbook{}, err
	}
	return s.store.ReviseRunbook(ctx, scope, id, base,
		RunbookInput{Definition: def, Actor: actor})
}

// CompileRunbook has the model compile an English playbook into a new
// runbook stored as an unsigned draft, with the redacted playbook and the
// model's name beside it. The playbook is redacted before the model sees
// it. A rejected draft (*runbook.Rejection) stores nothing.
func (s *Service) CompileRunbook(ctx context.Context, text, actor string) (Runbook,
	error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Runbook{}, err
	}
	clean := RedactText(text)
	if err := runbook.CheckSource(clean); err != nil {
		return Runbook{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	model := s.coord.model
	if model == nil || !model.IsEnabled() {
		return Runbook{}, fmt.Errorf("%w: compiling a playbook needs an LLM "+
			"(llm.enabled and sre.llm.enabled)", ErrModelUnavailable)
	}
	compiled, err := runbook.Compile(ctx, model, runbook.CompileRequest{Text: clean,
		Vocab: runbookVocab()})
	if errors.Is(err, runbook.ErrInvalid) {
		return Runbook{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err != nil {
		return Runbook{}, err
	}
	return s.store.CreateRunbook(ctx, scope, RunbookInput{Definition: compiled.Definition,
		Source: SourceCompiled, SourceText: clean, CompiledBy: model.Model(), Actor: actor})
}

// SignRunbook has signer (with role) sign version over the content hash
// they reviewed.
func (s *Service) SignRunbook(ctx context.Context, id UUID, version int, hash, signer,
	role string) (Runbook, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Runbook{}, err
	}
	return s.store.SignRunbook(ctx, scope, id, RunbookSignature{Version: version,
		ContentHash: hash, Signer: signer, Role: role})
}

// RetireRunbook stops a runbook for good.
func (s *Service) RetireRunbook(ctx context.Context, id UUID, actor string) (Runbook,
	error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Runbook{}, err
	}
	return s.store.RetireRunbook(ctx, scope, id, actor)
}

// RunbookRuns lists a runbook's runs, newest first.
func (s *Service) RunbookRuns(ctx context.Context, id UUID) ([]RunbookRun, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetRunbook(ctx, scope, id); err != nil {
		return nil, err
	}
	return s.store.RunbookRuns(ctx, scope, id, maxRunbookRuns)
}

// Similar lists the past investigations similar to one investigation of
// this database, under the same leakage guard as the model turn.
func (s *Service) Similar(ctx context.Context, id UUID) ([]SimilarIncident, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	inv, err := s.store.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	hs, err := s.store.Hypotheses(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	latest, _ := latestRevision(hs)
	items, err := s.store.SimilarInvestigations(ctx, scope, SimilarQuery{Target: inv,
		Family: inv.Summary.Family, Features: recordFeatures(latest, inv.Summary),
		Limit: maxSimilarListed})
	if err != nil {
		return nil, err
	}
	out := []SimilarIncident{}
	return out, redactInto(items, &out)
}

// RecordOutcome records an operator's verdict on a finished investigation.
func (s *Service) RecordOutcome(ctx context.Context, id UUID,
	req OutcomeRequest) (Outcome, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Outcome{}, err
	}
	return s.store.RecordOutcome(ctx, scope, id, req)
}
