package sre

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Durable model budget (Codex contracts §6). A reservation is taken in
// one transaction before provider I/O: the deployment/day advisory lock
// first, then the investigation row. Settled rows count their actual
// usage, cancelled rows nothing, and reserved/inflight/unknown rows their
// full allowance, so a crash or an uncertain call never frees budget.

const resColumns = `id::text, deployment_id::text, database_id::text,
	COALESCE(investigation_id::text, ''), request_key, state, input_reserved,
	output_reserved, reasoning_reserved, COALESCE(input_used, 0),
	COALESCE(output_used, 0), COALESCE(reasoning_used, 0), version`

// chargeExpr is what one reservation row costs against a budget (rows
// settled before the reasoning columns existed used no reasoning).
const chargeExpr = `CASE state WHEN 'settled' THEN COALESCE(%[1]s_used, 0)
	WHEN 'cancelled' THEN 0 ELSE %[1]s_reserved END`

func scanReservation(row pgx.Row) (Reservation, error) {
	var r Reservation
	var id, dep, db, inv, state string
	err := row.Scan(&id, &dep, &db, &inv, &r.RequestKey, &state, &r.Input, &r.Output,
		&r.Reasoning, &r.InputUsed, &r.OutputUsed, &r.ReasoningUsed, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	r.ID, r.InvestigationID, r.State = UUID(id), UUID(inv), ReservationState(state)
	r.Scope = Scope{DeploymentID: UUID(dep), DatabaseID: UUID(db)}
	return r, err
}

func (s *PostgresStore) validateTokens(req TokenRequest) error {
	if req.Input <= 0 || req.Output <= 0 || req.Reasoning < 0 {
		return fmt.Errorf("%w: token request must be positive", ErrInvalidRequest)
	}
	if req.Reasoning > s.limits.MaxReasoningTokens {
		return fmt.Errorf("%w: reasoning %d over the per-investigation cap %d",
			ErrBudgetExhausted, req.Reasoning, s.limits.MaxReasoningTokens)
	}
	if err := checkText("request key", req.RequestKey, true, 160); err != nil {
		return err
	}
	if req.Input > s.limits.MaxInputTokens || req.Output > s.limits.MaxOutputTokens {
		return fmt.Errorf("%w: request %d/%d over the per-investigation caps %d/%d",
			ErrBudgetExhausted, req.Input, req.Output, s.limits.MaxInputTokens,
			s.limits.MaxOutputTokens)
	}
	if s.limits.DatabaseDailyTokens == 0 || s.limits.DeploymentDailyTokens == 0 {
		return fmt.Errorf("%w: no daily allocation for model use", ErrBudgetExhausted)
	}
	return nil
}

// ReserveModel reserves one model turn for the leased investigation. A
// repeated request key returns the existing reservation.
func (s *PostgresStore) ReserveModel(ctx context.Context, lease Lease,
	req TokenRequest) (Reservation, error) {
	if err := lease.validate(); err != nil {
		return Reservation{}, err
	}
	if err := s.validateTokens(req); err != nil {
		return Reservation{}, err
	}
	var res Reservation
	err := s.inTx(ctx, "reserve model", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(
			'sre_budget:' || $1 || ':' || (clock_timestamp() AT TIME ZONE 'UTC')::date,
			0))`, string(lease.Scope.DeploymentID)); err != nil {
			return err
		}
		turns, err := s.lockLeasedTurns(ctx, tx, lease)
		if err != nil {
			return err
		}
		if existing, err := s.reservationByKey(ctx, tx, lease, req.RequestKey); err == nil {
			res = existing
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if turns >= s.limits.MaxModelTurns {
			return fmt.Errorf("%w: model turn limit %d reached", ErrBudgetExhausted,
				s.limits.MaxModelTurns)
		}
		if err := s.checkBudgets(ctx, tx, lease, req); err != nil {
			return err
		}
		res, err = s.insertReservation(ctx, tx, lease, req)
		return err
	})
	return res, err
}

func (s *PostgresStore) lockLeasedTurns(ctx context.Context, tx pgx.Tx,
	lease Lease) (int, error) {
	var turns int
	err := tx.QueryRow(ctx, `SELECT model_turns FROM sage.sre_investigations
		WHERE `+leaseGuard+` FOR UPDATE`, leaseArgs(lease)...).Scan(&turns)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLeaseLost
	}
	return turns, err
}

func (s *PostgresStore) reservationByKey(ctx context.Context, tx pgx.Tx, lease Lease,
	key string) (Reservation, error) {
	return scanReservation(tx.QueryRow(ctx, `SELECT `+resColumns+`
		FROM sage.sre_budget_reservations
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		  AND request_key = $4`, string(lease.Scope.DeploymentID),
		string(lease.Scope.DatabaseID), string(lease.InvestigationID), key))
}

// heldTokens is what reservations hold: this investigation's input,
// answer and reasoning tokens, and the database's and deployment's
// UTC-day totals.
type heldTokens struct {
	invIn, invOut, invReasoning, dbDay, depDay int64
}

func heldBy(ctx context.Context, tx pgx.Tx, lease Lease) (heldTokens, error) {
	in, out := fmt.Sprintf(chargeExpr, "input"), fmt.Sprintf(chargeExpr, "output")
	rsn := fmt.Sprintf(chargeExpr, "reasoning")
	all := in + ` + ` + out + ` + ` + rsn
	inv := ` FILTER (WHERE database_id = $2 AND investigation_id = $3), 0)::int8`
	var h heldTokens
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(`+in+`)`+inv+`,
		COALESCE(sum(`+out+`)`+inv+`, COALESCE(sum(`+rsn+`)`+inv+`,
		COALESCE(sum(`+all+`) FILTER (WHERE database_id = $2), 0)::int8,
		COALESCE(sum(`+all+`), 0)::int8
		FROM sage.sre_budget_reservations
		WHERE deployment_id = $1
		  AND utc_day = (clock_timestamp() AT TIME ZONE 'UTC')::date`,
		string(lease.Scope.DeploymentID), string(lease.Scope.DatabaseID),
		string(lease.InvestigationID)).Scan(&h.invIn, &h.invOut, &h.invReasoning,
		&h.dbDay, &h.depDay)
	return h, err
}

// checkBudgets checks the investigation aggregate (input, answer and
// reasoning separately) and the database and deployment UTC-day
// allocations, all callers included.
func (s *PostgresStore) checkBudgets(ctx context.Context, tx pgx.Tx, lease Lease,
	req TokenRequest) error {
	h, err := heldBy(ctx, tx, lease)
	if err != nil {
		return err
	}
	total := req.Input + req.Output + req.Reasoning
	switch {
	case h.invIn+req.Input > s.limits.MaxInputTokens,
		h.invOut+req.Output > s.limits.MaxOutputTokens:
		return fmt.Errorf("%w: investigation tokens %d/%d held of %d/%d",
			ErrBudgetExhausted, h.invIn, h.invOut, s.limits.MaxInputTokens,
			s.limits.MaxOutputTokens)
	case h.invReasoning+req.Reasoning > s.limits.MaxReasoningTokens:
		return fmt.Errorf("%w: investigation reasoning %d held of %d",
			ErrBudgetExhausted, h.invReasoning, s.limits.MaxReasoningTokens)
	case h.dbDay+total > s.limits.DatabaseDailyTokens:
		return fmt.Errorf("%w: database daily allocation %d of %d held",
			ErrBudgetExhausted, h.dbDay, s.limits.DatabaseDailyTokens)
	case h.depDay+total > s.limits.DeploymentDailyTokens:
		return fmt.Errorf("%w: deployment daily allocation %d of %d held",
			ErrBudgetExhausted, h.depDay, s.limits.DeploymentDailyTokens)
	}
	return nil
}

func (s *PostgresStore) insertReservation(ctx context.Context, tx pgx.Tx, lease Lease,
	req TokenRequest) (Reservation, error) {
	res, err := scanReservation(tx.QueryRow(ctx, `INSERT INTO sage.sre_budget_reservations
		(deployment_id, database_id, id, investigation_id, utc_day, caller_kind,
		 request_key, state, input_reserved, output_reserved, reasoning_reserved)
		VALUES ($1, $2, $3, $4, (clock_timestamp() AT TIME ZONE 'UTC')::date,
		        'sre_investigation', $5, 'reserved', $6, $7, $8)
		RETURNING `+resColumns, string(lease.Scope.DeploymentID),
		string(lease.Scope.DatabaseID), string(NewUUID()), string(lease.InvestigationID),
		req.RequestKey, req.Input, req.Output, req.Reasoning))
	if err != nil {
		return res, err
	}
	_, err = tx.Exec(ctx, `UPDATE sage.sre_investigations
		SET model_turns = model_turns + 1, version = version + 1,
		    updated_at = clock_timestamp()
		WHERE `+leaseGuard, leaseArgs(lease)...)
	return res, err
}

// MarkDispatched records that the call is about to reach the provider.
func (s *PostgresStore) MarkDispatched(ctx context.Context, scope Scope,
	res Reservation) (Reservation, error) {
	return s.moveReservation(ctx, scope, res, ReservationInflight,
		[]ReservationState{ReservationReserved}, Usage{})
}

// CancelModel releases a reservation proven never dispatched.
func (s *PostgresStore) CancelModel(ctx context.Context, scope Scope,
	res Reservation) (Reservation, error) {
	return s.moveReservation(ctx, scope, res, ReservationCancelled,
		[]ReservationState{ReservationReserved}, Usage{})
}

// SettleModel records known usage (settled) or keeps the full hold
// (unknown). Settling again is a no-op. Usage above the reservation is
// recorded as reported and returned with ErrUsageExceeded.
func (s *PostgresStore) SettleModel(ctx context.Context, scope Scope, res Reservation,
	u Usage) (Reservation, error) {
	if !u.Known {
		return s.moveReservation(ctx, scope, res, ReservationUnknown,
			[]ReservationState{ReservationReserved, ReservationInflight}, u)
	}
	if u.Input < 0 || u.Output < 0 || u.Reasoning < 0 {
		return res, fmt.Errorf("%w: negative usage", ErrInvalidRequest)
	}
	out, err := s.moveReservation(ctx, scope, res, ReservationSettled,
		[]ReservationState{ReservationReserved, ReservationInflight, ReservationUnknown}, u)
	if err == nil && (u.Input > out.Input || u.Output > out.Output ||
		u.Reasoning > out.Reasoning) {
		return out, fmt.Errorf("%w: used %d/%d/%d of %d/%d/%d (input/answer/reasoning)",
			ErrUsageExceeded, u.Input, u.Output, u.Reasoning, out.Input, out.Output,
			out.Reasoning)
	}
	return out, err
}

// moveReservation moves a reservation from one of the from states to
// to. Moving to the state it is already in is an idempotent no-op.
func (s *PostgresStore) moveReservation(ctx context.Context, scope Scope,
	res Reservation, to ReservationState, from []ReservationState,
	u Usage) (Reservation, error) {
	if err := validateIDs(scope, res.ID); err != nil {
		return res, err
	}
	states := make([]string, len(from))
	for i, f := range from {
		states[i] = string(f)
	}
	args := []any{string(scope.DeploymentID), string(scope.DatabaseID), string(res.ID)}
	out, err := scanReservation(s.pool.QueryRow(ctx, `UPDATE sage.sre_budget_reservations
		SET state = $4, version = version + 1,
		    input_used = CASE WHEN $4 = 'settled' THEN $5::int8 ELSE input_used END,
		    output_used = CASE WHEN $4 = 'settled' THEN $6::int8 ELSE output_used END,
		    reasoning_used = CASE WHEN $4 = 'settled' THEN $8::int8
		                          ELSE reasoning_used END,
		    settled_at = CASE WHEN $4 IN ('settled', 'cancelled')
		                      THEN clock_timestamp() ELSE settled_at END
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3 AND state = ANY($7)
		RETURNING `+resColumns, append(args, string(to), u.Input, u.Output, states,
		u.Reasoning)...))
	if !errors.Is(err, ErrNotFound) {
		return out, storeErr(ctx, "move reservation", err)
	}
	cur, err := scanReservation(s.pool.QueryRow(ctx, `SELECT `+resColumns+`
		FROM sage.sre_budget_reservations
		WHERE deployment_id = $1 AND database_id = $2 AND id = $3`, args...))
	if err != nil {
		return res, storeErr(ctx, "move reservation", err)
	}
	if cur.State == to {
		return cur, nil
	}
	return cur, fmt.Errorf("%w: reservation %s -> %s", ErrInvalidTransition, cur.State, to)
}
