package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The tool-calling investigator's durable token budget (roadmap 2.1).
// Each model call is reserved before provider I/O in the same ledger as
// the review turn (sage.sre_budget_reservations), under its own caller
// kind: against the plan's per-investigation cap (every run of the
// investigation counted, so a resumed run cannot spend it twice) and the
// database's and deployment's UTC-day allocations, which every caller
// shares. It does not use the review turn's two-turn counter; the plan's
// step budget bounds the calls.

// InvestigatorCallerKind marks the investigator's reservations.
const InvestigatorCallerKind = "sre_investigator"

// InvestigatorStore is the store surface the investigator needs beyond
// the coordinator's.
type InvestigatorStore interface {
	ReserveInvestigator(ctx context.Context, lease Lease, req TokenRequest,
		capTokens int64) (Reservation, error)
}

var _ InvestigatorStore = (*PostgresStore)(nil)

// investigatorStore is the coordinator store's investigator surface,
// required when the investigator is configured.
func investigatorStore(d CoordinatorDeps) (InvestigatorStore, error) {
	if d.Investigator == nil {
		return nil, nil
	}
	if err := d.Investigator.Validate(); err != nil {
		return nil, err
	}
	st, ok := d.Store.(InvestigatorStore)
	if !ok {
		return nil, fmt.Errorf("%w: the investigator needs a store with a durable "+
			"token budget", ErrInvalidRequest)
	}
	return st, nil
}

// ReserveInvestigator reserves one investigator model call for the
// leased investigation; capTokens is the plan's per-investigation cap.
// A repeated request key returns the existing reservation.
func (s *PostgresStore) ReserveInvestigator(ctx context.Context, lease Lease,
	req TokenRequest, capTokens int64) (Reservation, error) {
	if err := lease.validate(); err != nil {
		return Reservation{}, err
	}
	if err := s.validateInvestigatorTokens(req, capTokens); err != nil {
		return Reservation{}, err
	}
	var res Reservation
	err := s.inTx(ctx, "reserve investigator", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(
			'sre_budget:' || $1 || ':' || (clock_timestamp() AT TIME ZONE 'UTC')::date,
			0))`, string(lease.Scope.DeploymentID)); err != nil {
			return err
		}
		if _, err := s.lockLeasedTurns(ctx, tx, lease); err != nil {
			return err
		}
		if existing, err := s.reservationByKey(ctx, tx, lease, req.RequestKey); err == nil {
			res = existing
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err := s.checkInvestigatorBudgets(ctx, tx, lease, req, capTokens); err != nil {
			return err
		}
		var err error
		res, err = insertInvestigatorReservation(ctx, tx, lease, req)
		return err
	})
	return res, err
}

func (s *PostgresStore) validateInvestigatorTokens(req TokenRequest, capTokens int64) error {
	switch {
	case req.Input <= 0 || req.Output <= 0 || req.Reasoning < 0:
		return fmt.Errorf("%w: token request must be positive", ErrInvalidRequest)
	case capTokens <= 0 || capTokens > CeilingInvestigatorTokens:
		return fmt.Errorf("%w: investigator cap %d outside [1, %d]", ErrInvalidRequest,
			capTokens, CeilingInvestigatorTokens)
	case s.limits.DatabaseDailyTokens == 0 || s.limits.DeploymentDailyTokens == 0:
		return fmt.Errorf("%w: no daily allocation for model use", ErrBudgetExhausted)
	}
	return checkText("request key", req.RequestKey, true, 160)
}

// checkInvestigatorBudgets checks this investigation's investigator
// tokens against the plan's cap and the daily allocations.
func (s *PostgresStore) checkInvestigatorBudgets(ctx context.Context, tx pgx.Tx,
	lease Lease, req TokenRequest, capTokens int64) error {
	h, err := heldBy(ctx, tx, lease)
	if err != nil {
		return err
	}
	all := fmt.Sprintf(chargeExpr, "input") + " + " + fmt.Sprintf(chargeExpr, "output") +
		" + " + fmt.Sprintf(chargeExpr, "reasoning")
	var held int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(`+all+`), 0)::int8
		FROM sage.sre_budget_reservations
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		  AND caller_kind = $4`, string(lease.Scope.DeploymentID),
		string(lease.Scope.DatabaseID), string(lease.InvestigationID),
		InvestigatorCallerKind).Scan(&held); err != nil {
		return err
	}
	total := req.Input + req.Output + req.Reasoning
	switch {
	case held+total > capTokens:
		return fmt.Errorf("%w: investigator tokens %d held of %d", ErrBudgetExhausted, held,
			capTokens)
	case h.dbDay+total > s.limits.DatabaseDailyTokens:
		return fmt.Errorf("%w: database daily allocation %d of %d held",
			ErrBudgetExhausted, h.dbDay, s.limits.DatabaseDailyTokens)
	case h.depDay+total > s.limits.DeploymentDailyTokens:
		return fmt.Errorf("%w: deployment daily allocation %d of %d held",
			ErrBudgetExhausted, h.depDay, s.limits.DeploymentDailyTokens)
	}
	return nil
}

func insertInvestigatorReservation(ctx context.Context, tx pgx.Tx, lease Lease,
	req TokenRequest) (Reservation, error) {
	return scanReservation(tx.QueryRow(ctx, `INSERT INTO sage.sre_budget_reservations
		(deployment_id, database_id, id, investigation_id, utc_day, caller_kind,
		 request_key, state, input_reserved, output_reserved, reasoning_reserved)
		VALUES ($1, $2, $3, $4, (clock_timestamp() AT TIME ZONE 'UTC')::date,
		        $5, $6, 'reserved', $7, $8, $9)
		RETURNING `+resColumns, string(lease.Scope.DeploymentID),
		string(lease.Scope.DatabaseID), string(NewUUID()), string(lease.InvestigationID),
		InvestigatorCallerKind, req.RequestKey, req.Input, req.Output, req.Reasoning))
}
