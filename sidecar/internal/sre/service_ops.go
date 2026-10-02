package sre

import (
	"context"
	"fmt"
)

// Operator control of investigations (AI-SRE-SPEC §4 R1, §9): start an
// investigation of a case, stop it (a resumable pause: steps, evidence
// and consumed budget are kept) and resume it. Stop and resume carry the
// version the operator saw (If-Match); every change is attributed to the
// operator. None of these touch the monitored database.

// Start creates (or coalesces into) an investigation on an operator's
// request and queues it. t.Actor is required.
func (s *Service) Start(ctx context.Context, t Trigger) (Investigation, bool, error) {
	if s == nil || s.coord == nil || s.store == nil {
		return Investigation{}, false, fmt.Errorf("%w: investigations are not configured",
			ErrMetadataUnavailable)
	}
	if err := checkText("actor", t.Actor, true, 128); err != nil {
		return Investigation{}, false, err
	}
	if _, err := s.scope(ctx); err != nil {
		return Investigation{}, false, err
	}
	inv, created, err := s.coord.Start(ctx, t)
	if err != nil {
		return Investigation{}, false, err
	}
	var clean Investigation
	return clean, created, redactInto(inv, &clean)
}

// Stop pauses a live investigation at the version the operator saw.
func (s *Service) Stop(ctx context.Context, id UUID, version int64,
	actor string) (Investigation, error) {
	return s.transition(ctx, id, version, StatePaused, actor)
}

// Resume re-queues a stopped (paused) investigation and hands it to the
// coordinator's worker.
func (s *Service) Resume(ctx context.Context, id UUID, version int64,
	actor string) (Investigation, error) {
	inv, err := s.transition(ctx, id, version, StateQueued, actor)
	if err == nil {
		s.coord.enqueue(inv.ID)
	}
	return inv, err
}

func (s *Service) transition(ctx context.Context, id UUID, version int64, to State,
	actor string) (Investigation, error) {
	scope, err := s.scope(ctx)
	if err != nil {
		return Investigation{}, err
	}
	inv, err := s.store.OperatorTransition(ctx, scope, id, version, to, actor)
	s.coord.durability.Observe(err)
	if err != nil {
		return Investigation{}, err
	}
	var clean Investigation
	return clean, redactInto(inv, &clean)
}
