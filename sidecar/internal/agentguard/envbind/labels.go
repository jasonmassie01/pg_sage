package envbind

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultSetBy names pg_sage as the writer of a default (prod) label row.
const DefaultSetBy = "pg_sage"

// querier is a pool or a transaction on the control database.
type querier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const labelColumns = `database_id::text, label, identity, verified, set_by, set_at,
	observed, observed_at, COALESCE(pending_label, ''), COALESCE(pending_by, ''), pending_at`

func scanLabel(row pgx.Row, extra ...any) (LabelRecord, error) {
	var r LabelRecord
	var identity, observed []byte
	dest := []any{&r.DatabaseID, &r.Label, &identity, &r.Verified, &r.SetBy, &r.SetAt,
		&observed, &r.ObservedAt, &r.PendingLabel, &r.PendingBy, &r.PendingAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return LabelRecord{}, err
	}
	if err := json.Unmarshal(identity, &r.Identity); err != nil {
		return LabelRecord{}, fmt.Errorf("decode label identity of %s: %w", r.DatabaseID, err)
	}
	if observed != nil {
		var o Identity
		if err := json.Unmarshal(observed, &o); err != nil {
			return LabelRecord{}, fmt.Errorf("decode observed identity of %s: %w",
				r.DatabaseID, err)
		}
		r.Observed = &o
	}
	return r, nil
}

// readRow returns the label row of a database, if any. forUpdate locks it.
func readRow(ctx context.Context, q querier, id string, forUpdate bool) (*LabelRecord,
	error) {
	sql := `/* pg_sage agent_env_label v1 */ SELECT ` + labelColumns +
		` FROM sage.guard_environment_labels WHERE database_id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	r, err := scanLabel(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read environment label of %s: %w", id, err)
	}
	return &r, nil
}

// peersSQL reads the other databases with the same physical key, by their
// last observed identity (else the identity of their label).
const peersSQL = `/* pg_sage agent_env_label v1 */
SELECT database_id::text, label, COALESCE(observed, identity)
FROM sage.guard_environment_labels
WHERE database_id <> $1
  AND COALESCE(observed, identity)->>'system_identifier' = $2
  AND COALESCE(observed, identity)->>'db_oid' = $3`

func readPeers(ctx context.Context, q querier, id string, live Identity) ([]Peer, error) {
	if live.PhysicalKey() == "" {
		return nil, nil
	}
	rows, err := q.Query(ctx, peersSQL, id, live.SystemIdentifier,
		strconv.FormatUint(uint64(live.DBOID), 10))
	if err != nil {
		return nil, fmt.Errorf("read databases sharing %s: %w", live.PhysicalKey(), err)
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		var p Peer
		var raw []byte
		if err := rows.Scan(&p.DatabaseID, &p.Label, &raw); err != nil {
			return nil, fmt.Errorf("read databases sharing %s: %w", live.PhysicalKey(), err)
		}
		if err := json.Unmarshal(raw, &p.Identity); err != nil {
			return nil, fmt.Errorf("decode identity of %s: %w", p.DatabaseID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const ensureRowSQL = `/* pg_sage agent_env_label v1 */
INSERT INTO sage.guard_environment_labels (database_id, identity, set_by, observed,
    observed_at)
VALUES ($1, $2, '` + DefaultSetBy + `', $2, now())
ON CONFLICT (database_id) DO NOTHING`

const observeSQL = `/* pg_sage agent_env_label v1 */
UPDATE sage.guard_environment_labels SET observed = $2, observed_at = now()
WHERE database_id = $1 AND observed IS DISTINCT FROM $2`

// observeRow records the live identity of a database: a default prod row
// when it has none, else the observed tuple when it changed.
func observeRow(ctx context.Context, q querier, id string, live Identity) error {
	raw, err := json.Marshal(live)
	if err != nil {
		return fmt.Errorf("encode identity of %s: %w", id, err)
	}
	if _, err := q.Exec(ctx, ensureRowSQL, id, raw); err != nil {
		return fmt.Errorf("record environment label of %s: %w", id, err)
	}
	if _, err := q.Exec(ctx, observeSQL, id, raw); err != nil {
		return fmt.Errorf("record observed identity of %s: %w", id, err)
	}
	return nil
}

const applySQL = `/* pg_sage agent_env_label v1 */
UPDATE sage.guard_environment_labels SET label = $2, identity = $3, verified = $4,
    set_by = $5, set_at = now(), observed = $3, observed_at = now(),
    pending_label = NULL, pending_by = NULL, pending_at = NULL
WHERE database_id = $1
RETURNING ` + labelColumns

const pendingSQL = `/* pg_sage agent_env_label v1 */
UPDATE sage.guard_environment_labels SET pending_label = $2, pending_by = $3,
    pending_at = now()
WHERE database_id = $1
RETURNING ` + labelColumns

func applyLabel(ctx context.Context, q querier, id string, label Env, live Identity,
	actor string) (LabelRecord, error) {
	raw, err := json.Marshal(live)
	if err != nil {
		return LabelRecord{}, fmt.Errorf("encode identity of %s: %w", id, err)
	}
	r, err := scanLabel(q.QueryRow(ctx, applySQL, id, label, raw, label != EnvProd, actor))
	if err != nil {
		return LabelRecord{}, fmt.Errorf("set environment label of %s: %w", id, err)
	}
	return r, nil
}

func setPending(ctx context.Context, q querier, id string, label Env, actor string) (
	LabelRecord, error) {
	r, err := scanLabel(q.QueryRow(ctx, pendingSQL, id, label, actor))
	if err != nil {
		return LabelRecord{}, fmt.Errorf("record pending label of %s: %w", id, err)
	}
	return r, nil
}

// pendingFresh reports whether the row's pending request is younger than
// ttl on the control database's clock.
func pendingFresh(ctx context.Context, q querier, id string, ttl time.Duration) (bool,
	error) {
	var fresh bool
	err := q.QueryRow(ctx, `/* pg_sage agent_env_label v1 */
		SELECT COALESCE(pending_at > now() - $2::float8 * interval '1 second', false)
		FROM sage.guard_environment_labels WHERE database_id = $1`, id,
		ttl.Seconds()).Scan(&fresh)
	if err != nil {
		return false, fmt.Errorf("read pending label of %s: %w", id, err)
	}
	return fresh, nil
}
