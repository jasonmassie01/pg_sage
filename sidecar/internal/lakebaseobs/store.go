package lakebaseobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationVersion is independent of pg_sage bootstrap migrations. EnsureSchema
// must run against the persistent metadata/control pool before starting workers.
const MigrationVersion = 6
const ddl = `
CREATE SCHEMA IF NOT EXISTS sage;
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_schema(version integer PRIMARY KEY);
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_cursors(
 binding text NOT NULL, stream text NOT NULL, revision bigint NOT NULL DEFAULT 0,
 state jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(binding,stream));
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_inbox(
 binding text NOT NULL, event_id text NOT NULL, stream text NOT NULL,
 occurred_at timestamptz NOT NULL, received_at timestamptz NOT NULL,
 payload_until timestamptz NOT NULL, identity_until timestamptz NOT NULL,
 event jsonb, PRIMARY KEY(binding,event_id));
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_pending
 ON sage.lakebaseobs_inbox(binding,stream,occurred_at,event_id);
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_delivery
 ON sage.lakebaseobs_inbox(binding,occurred_at,event_id) INCLUDE(payload_until)
 WHERE event IS NOT NULL;
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_payload_expiry
 ON sage.lakebaseobs_inbox(payload_until,binding,event_id) WHERE event IS NOT NULL;
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_identity_expiry
 ON sage.lakebaseobs_inbox(identity_until,binding,event_id);
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_acks(
 binding text NOT NULL, event_id text NOT NULL, consumer text NOT NULL,
 acknowledged_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(binding,event_id,consumer),
 FOREIGN KEY(binding,event_id) REFERENCES sage.lakebaseobs_inbox(binding,event_id)
  ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_owners(
 deployment_id uuid NOT NULL, database_id uuid NOT NULL, scope_hash text NOT NULL,
 policy_binding text NOT NULL DEFAULT '',
 registered_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(deployment_id,database_id,scope_hash));
ALTER TABLE sage.lakebaseobs_owners
 ADD COLUMN IF NOT EXISTS policy_binding text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_owner_policy(
 deployment_id uuid NOT NULL, database_id uuid NOT NULL,
 allowed_scopes text[] NOT NULL DEFAULT '{}', allow_payload boolean NOT NULL DEFAULT false,
 policy_epoch timestamptz NOT NULL DEFAULT 'epoch', updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(deployment_id,database_id));
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_attempts(
 binding text NOT NULL, event_id text NOT NULL, consumer text NOT NULL,
 attempts integer NOT NULL CHECK(attempts>0), next_at timestamptz NOT NULL,
 last_error text NOT NULL DEFAULT '',
 PRIMARY KEY(binding,event_id,consumer),
 FOREIGN KEY(binding,event_id) REFERENCES sage.lakebaseobs_inbox(binding,event_id)
  ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_delivery_stream
 ON sage.lakebaseobs_inbox(binding,stream,occurred_at,event_id) INCLUDE(payload_until)
 WHERE event IS NOT NULL;
INSERT INTO sage.lakebaseobs_schema(version)VALUES(1),(2),(3),(4),(5),(6)ON CONFLICT DO NOTHING;`

type Store struct {
	pool        *pgxpool.Pool
	policyEpoch time.Time
}

func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("lakebase telemetry persistent control pool required")
	}
	return &Store{pool: pool}, nil
}

// WithPolicyEpoch binds policy mutations to this process's shared startup UTC.
// Scope hashes remain stable. Cross-process ordering assumes synchronized clocks;
// the zero/default epoch preserves legacy standalone callers.
func (s *Store) WithPolicyEpoch(epoch time.Time) *Store {
	copy := *s
	copy.policyEpoch = epoch.UTC().Truncate(time.Microsecond)
	return &copy
}
func (s *Store) epoch() time.Time {
	if s.policyEpoch.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return s.policyEpoch
}
func (s *Store) EnsureSchema(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	const lock = "SELECT pg_advisory_xact_lock(hashtextextended('sage.lakebaseobs_schema',0))"
	if _, err = tx.Exec(ctx, lock); err != nil {
		return err
	}
	// The advisory lock may wait; table locks for the DDL must not.
	if _, err = tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("initialize Lakebase telemetry schema: %w", err)
	}
	var version int
	err = tx.QueryRow(ctx, "SELECT max(version) FROM sage.lakebaseobs_schema").Scan(&version)
	if err != nil {
		return err
	}
	if version != MigrationVersion {
		return ErrSchema
	}
	return tx.Commit(ctx)
}

// Cursor tracks reads, never provider completeness. Pipeline binding includes
// payload policy, so enabling payload starts a separate historical backfill.
type Cursor struct {
	Revision         int64     `json:"-"`
	ForwardThrough   time.Time `json:"forward_through"`
	ReconcileThrough time.Time `json:"reconcile_through"`
	ReconcileTarget  time.Time `json:"reconcile_target"`
	LastPoll         time.Time `json:"last_poll"`
	LastEvent        time.Time `json:"last_event"`
	GapCount         int64     `json:"gap_count"`
	LastGap          string    `json:"last_gap,omitempty"`
	// Width is the adaptive forward read width; zero means Options.Window.
	Width time.Duration `json:"width,omitempty"`
	// Sparse counts consecutive forward reads projected under MaxRows/4.
	Sparse int `json:"sparse,omitempty"`
	// ShortThrough/ShortTarget track the short reconcile sweep;
	// ReconcileThrough/ReconcileTarget track the deep sweep begun at DeepStartedAt.
	ShortThrough  time.Time `json:"short_through,omitzero"`
	ShortTarget   time.Time `json:"short_target,omitzero"`
	DeepStartedAt time.Time `json:"deep_started_at,omitzero"`
}

func (s *Store) LoadCursor(ctx context.Context, binding string, stream Stream) (Cursor, error) {
	var c Cursor
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT revision,state FROM sage.lakebaseobs_cursors
WHERE binding=$1 AND stream=$2`, binding, string(stream)).Scan(&c.Revision, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if json.Unmarshal(raw, &c) != nil {
		return c, ErrSchema
	}
	return c, nil
}

// CommitBatch atomically persists deduplicated events and compare-and-swaps the
// cursor. Duplicate occurrence multiplicity is unknown, not silently exact.
func (s *Store) CommitBatch(ctx context.Context, binding string, stream Stream,
	before, after Cursor, events []Event, retention time.Duration) error {
	_, err := s.CommitBatchAcked(ctx, binding, stream, before, after, events, retention, nil)
	return err
}

var consumerName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

// History serves only this pipeline's retained events, never an account-wide
// warehouse read. Callers must authorize the registered database before use.
// It is bounded historical context, not proof of source completeness.
func (s *Store) History(ctx context.Context, binding string, stream Stream, from, to time.Time,
	maxRows int, now time.Time) ([]Event, error) {
	if binding == "" || maxRows < 1 || maxRows > 1000 {
		return nil, ErrScope
	}
	if _, ok := specs[stream]; !ok {
		return nil, ErrSchema
	}
	if err := (Window{from, to}).Validate(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT event FROM sage.lakebaseobs_inbox
WHERE binding=$1 AND stream=$2 AND occurred_at >= $3 AND occurred_at < $4
AND event IS NOT NULL AND payload_until > $5
ORDER BY occurred_at DESC,event_id LIMIT $6`, binding, string(stream), from, to, now, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var raw []byte
		var e Event
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &e) != nil {
			return nil, ErrSchema
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) Ack(ctx context.Context, binding, eventID, consumer string) error {
	if !consumerName.MatchString(consumer) {
		return ErrScope
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer)
VALUES($1,$2,$3)ON CONFLICT DO NOTHING`, binding, eventID, consumer)
	return err
}

// DeliverAndAck re-reads a pending event under the same shared policy fence as
// its sink commit. A previously loaded Pending value cannot bypass revocation.
// Consumers must still make their sink idempotent by event ID across crashes.
func (s *Store) DeliverAndAck(ctx context.Context, binding, eventID string, consumer Consumer,
	now time.Time) error {
	if consumer == nil || !consumerName.MatchString(consumer.Name()) {
		return ErrScope
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = s.lockDeliveryPolicy(ctx, tx, binding, nil); err != nil {
		return err
	}
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT i.event FROM sage.lakebaseobs_inbox i
WHERE i.binding=$1 AND i.event_id=$2 AND i.event IS NOT NULL AND i.payload_until>$3
AND NOT EXISTS(SELECT 1 FROM sage.lakebaseobs_acks a
WHERE a.binding=i.binding AND a.event_id=i.event_id AND a.consumer=$4)
FOR SHARE OF i`, binding, eventID, now, consumer.Name()).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var e Event
	if json.Unmarshal(raw, &e) != nil {
		return ErrSchema
	}
	if err = s.lockDeliveryPolicy(ctx, tx, binding, []Event{e}); err != nil {
		return err
	}
	if err = consumer.Deliver(ctx, e); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer)
VALUES($1,$2,$3)ON CONFLICT DO NOTHING`, binding, eventID, consumer.Name()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) checkPolicy(ctx context.Context, binding string, payload bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	probe := Event{ScopeHash: strings.SplitN(binding, ":", 2)[0], PayloadAllowed: payload}
	return s.lockDeliveryPolicy(ctx, tx, binding, []Event{probe})
}
