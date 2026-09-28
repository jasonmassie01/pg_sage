package sre

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The event chain (AI-SRE-SPEC §10): every change to an investigation
// appends one event whose hash covers the previous hash, the sequence,
// type, actor, database-clock time and payload. Hashing happens in SQL
// so appending and verifying use the same canonical text (jsonb::text).
// A database administrator can still rewrite rows; the chain detects a
// rewrite, it does not prevent one.

// eventDigest is the text an event's hash covers, in column form.
const eventDigest = `concat_ws('|', sequence, event_type, actor,
	to_char(observed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), payload::text)`

const appendEventSQL = `WITH prev AS (
    SELECT sequence, hash FROM sage.sre_events
    WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
    ORDER BY sequence DESC LIMIT 1
), link AS (
    SELECT COALESCE((SELECT p.sequence FROM prev p), 0) + 1 AS sequence,
           (SELECT p.hash FROM prev p) AS previous_hash,
           $4::text AS event_type, $5::text AS actor,
           clock_timestamp() AS observed_at, $6::jsonb AS payload
)
INSERT INTO sage.sre_events (deployment_id, database_id, investigation_id, sequence,
    event_type, actor, observed_at, payload, previous_hash, hash)
SELECT $1, $2, $3, sequence, event_type, actor, observed_at, payload, previous_hash,
       sha256(COALESCE(previous_hash, ''::bytea) || convert_to(` + eventDigest + `, 'UTF8'))
FROM link`

// appendEvent adds one link. Callers hold the investigation row lock, so
// links of one investigation are appended one at a time.
func appendEvent(ctx context.Context, tx pgx.Tx, scope Scope, id UUID, typ,
	actor string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("%w: event payload: %v", ErrInvalidRequest, err)
	}
	_, err = tx.Exec(ctx, appendEventSQL, string(scope.DeploymentID),
		string(scope.DatabaseID), string(id), typ, actor, string(raw))
	return err
}

func workerActor(l Lease) string { return "worker:" + string(l.WorkerID) }

// Events lists an investigation's hash chain in order.
func (s *PostgresStore) Events(ctx context.Context, scope Scope, id UUID) ([]Event, error) {
	if err := validateIDs(scope, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT sequence, event_type, actor, observed_at,
		    payload::text, previous_hash, hash
		FROM sage.sre_events
		WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3
		ORDER BY sequence`, string(scope.DeploymentID), string(scope.DatabaseID),
		string(id))
	if err != nil {
		return nil, storeErr(ctx, "events", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.Sequence, &e.Type, &e.Actor, &e.ObservedAt, &payload,
			&e.PreviousHash, &e.Hash); err != nil {
			return nil, storeErr(ctx, "events", err)
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, storeErr(ctx, "events", rows.Err())
}

// VerifyEvents recomputes every hash and link of the chain. A missing,
// reordered or rewritten link is ErrChainBroken; so is an existing
// investigation without events.
func (s *PostgresStore) VerifyEvents(ctx context.Context, scope Scope, id UUID) error {
	if _, err := s.Get(ctx, scope, id); err != nil {
		return err
	}
	var total, bad int
	err := s.pool.QueryRow(ctx, `SELECT count(*),
		    count(*) FILTER (WHERE NOT (self_ok AND link_ok AND seq_ok))
		FROM (SELECT hash = sha256(COALESCE(previous_hash, ''::bytea) ||
		                 convert_to(`+eventDigest+`, 'UTF8')) AS self_ok,
		             previous_hash IS NOT DISTINCT FROM
		                 lag(hash) OVER (ORDER BY sequence) AS link_ok,
		             sequence = row_number() OVER (ORDER BY sequence) AS seq_ok
		      FROM sage.sre_events
		      WHERE deployment_id = $1 AND database_id = $2 AND investigation_id = $3) c`,
		string(scope.DeploymentID), string(scope.DatabaseID), string(id)).
		Scan(&total, &bad)
	if err != nil {
		return storeErr(ctx, "verify events", err)
	}
	if total == 0 || bad > 0 {
		return fmt.Errorf("%w: %d of %d links fail verification", ErrChainBroken,
			bad, total)
	}
	return nil
}
