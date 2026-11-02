package lakebaseobs

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationVersion is independent of pg_sage bootstrap migrations. EnsureSchema
// must run against the persistent metadata/control pool before starting workers.
const MigrationVersion = 5
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
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_pending ON sage.lakebaseobs_inbox(binding,stream,occurred_at,event_id);
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_delivery ON sage.lakebaseobs_inbox(binding,occurred_at,event_id) INCLUDE(payload_until) WHERE event IS NOT NULL;
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_payload_expiry ON sage.lakebaseobs_inbox(payload_until,binding,event_id) WHERE event IS NOT NULL;
CREATE INDEX IF NOT EXISTS lakebaseobs_inbox_identity_expiry ON sage.lakebaseobs_inbox(identity_until,binding,event_id);
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_acks(
 binding text NOT NULL, event_id text NOT NULL, consumer text NOT NULL,
 acknowledged_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(binding,event_id,consumer),
 FOREIGN KEY(binding,event_id) REFERENCES sage.lakebaseobs_inbox(binding,event_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_owners(
 deployment_id uuid NOT NULL, database_id uuid NOT NULL, scope_hash text NOT NULL,
 policy_binding text NOT NULL DEFAULT '',
 registered_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(deployment_id,database_id,scope_hash));
ALTER TABLE sage.lakebaseobs_owners ADD COLUMN IF NOT EXISTS policy_binding text NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS sage.lakebaseobs_owner_policy(
 deployment_id uuid NOT NULL, database_id uuid NOT NULL,
 allowed_scopes text[] NOT NULL DEFAULT '{}', allow_payload boolean NOT NULL DEFAULT false,
 policy_epoch timestamptz NOT NULL DEFAULT 'epoch', updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(deployment_id,database_id));
INSERT INTO sage.lakebaseobs_schema(version)VALUES(1),(2),(3),(4),(5)ON CONFLICT DO NOTHING;`

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
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('sage.lakebaseobs_schema',0))"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("initialize Lakebase telemetry schema: %w", err)
	}
	var version int
	if err = tx.QueryRow(ctx, "SELECT max(version) FROM sage.lakebaseobs_schema").Scan(&version); err != nil {
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
}

func (s *Store) LoadCursor(ctx context.Context, binding string, stream Stream) (Cursor, error) {
	var c Cursor
	var raw []byte
	err := s.pool.QueryRow(ctx, "SELECT revision,state FROM sage.lakebaseobs_cursors WHERE binding=$1 AND stream=$2", binding, string(stream)).Scan(&c.Revision, &raw)
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
func (s *Store) CommitBatch(ctx context.Context, binding string, stream Stream, before, after Cursor, events []Event, retention time.Duration) error {
	if binding == "" || retention <= 0 || retention > SourceRetention {
		return ErrScope
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = s.lockDeliveryPolicy(ctx, tx, binding, events); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO sage.lakebaseobs_cursors(binding,stream,revision,state)VALUES($1,$2,0,'{}')ON CONFLICT DO NOTHING", binding, string(stream))
	if err != nil {
		return err
	}
	var revision int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM sage.lakebaseobs_cursors WHERE binding=$1 AND stream=$2 FOR UPDATE", binding, string(stream)).Scan(&revision); err != nil {
		return err
	}
	if revision != before.Revision {
		return ErrConflict
	}
	for _, e := range events {
		if e.ID == "" || e.Stream != stream || e.Version != SchemaVersion || e.OccurredAt.IsZero() || e.ReceivedAt.IsZero() {
			return ErrMalformed
		}
		raw, jerr := json.Marshal(e)
		if jerr != nil {
			return jerr
		}
		_, err = tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_inbox(binding,event_id,stream,occurred_at,received_at,payload_until,identity_until,event)
VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb)ON CONFLICT(binding,event_id)DO NOTHING`, binding, e.ID, string(stream), e.OccurredAt, e.ReceivedAt, e.OccurredAt.Add(retention), e.OccurredAt.Add(SourceRetention+24*time.Hour), raw)
		if err != nil {
			return err
		}
	}
	raw, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "UPDATE sage.lakebaseobs_cursors SET revision=revision+1,state=$3::jsonb,updated_at=now() WHERE binding=$1 AND stream=$2", binding, string(stream), raw)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var consumerName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,80}$`)

// Select identities before reading JSON documents: an ACK-heavy backlog can
// walk the covering index without fetching every already delivered payload.
// There is no consumer watermark: a late historical event is still eligible.
const pendingSQL = `WITH pending AS MATERIALIZED (
 SELECT i.binding,i.event_id,i.occurred_at FROM sage.lakebaseobs_inbox i
 WHERE i.binding=$1 AND i.event IS NOT NULL AND i.payload_until>$3
 AND NOT EXISTS(SELECT 1 FROM sage.lakebaseobs_acks a WHERE a.binding=i.binding AND a.event_id=i.event_id AND a.consumer=$2)
 ORDER BY i.occurred_at,i.event_id LIMIT $4)
SELECT (SELECT i.event FROM sage.lakebaseobs_inbox i WHERE i.binding=p.binding AND i.event_id=p.event_id)
FROM pending p ORDER BY p.occurred_at,p.event_id`

func (s *Store) Pending(ctx context.Context, binding, consumer string, limit int, now time.Time) ([]Event, error) {
	if !consumerName.MatchString(consumer) || limit < 1 || limit > 10000 {
		return nil, ErrScope
	}
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	tx, err := beginStoreOperation(ctx, s.pool, true)
	if err != nil {
		return nil, err
	}
	defer rollbackStoreOperation(tx)
	rows, err := tx.Query(ctx, pendingSQL, binding, consumer, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
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

// History serves only this pipeline's retained events, never an account-wide
// warehouse read. Callers must authorize the registered database before use.
// It is bounded historical context, not proof of source completeness.
func (s *Store) History(ctx context.Context, binding string, stream Stream, from, to time.Time, maxRows int, now time.Time) ([]Event, error) {
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
	_, err := s.pool.Exec(ctx, "INSERT INTO sage.lakebaseobs_acks(binding,event_id,consumer)VALUES($1,$2,$3)ON CONFLICT DO NOTHING", binding, eventID, consumer)
	return err
}

// DeliverAndAck re-reads a pending event under the same shared policy fence as
// its sink commit. A previously loaded Pending value cannot bypass revocation.
// Consumers must still make their sink idempotent by event ID across crashes.
func (s *Store) DeliverAndAck(ctx context.Context, binding, eventID string, consumer Consumer, now time.Time) error {
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
AND NOT EXISTS(SELECT 1 FROM sage.lakebaseobs_acks a WHERE a.binding=i.binding AND a.event_id=i.event_id AND a.consumer=$4)
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
	return s.lockDeliveryPolicy(ctx, tx, binding, []Event{{ScopeHash: strings.SplitN(binding, ":", 2)[0], PayloadAllowed: payload}})
}

// EnforceRetention also tightens copies from an earlier payload/source policy
// epoch. Increasing a policy never resurrects already erased content.
func (s *Store) EnforceRetention(ctx context.Context, scopeHash string, retention time.Duration) error {
	if len(scopeHash) != 64 || retention <= 0 || retention > SourceRetention {
		return ErrScope
	}
	if _, err := hex.DecodeString(scopeHash); err != nil {
		return ErrScope
	}
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	tx, err := beginStoreOperation(ctx, s.pool, false)
	if err != nil {
		return err
	}
	defer rollbackStoreOperation(tx)
	_, err = tx.Exec(ctx, `UPDATE sage.lakebaseobs_inbox
SET payload_until = occurred_at + make_interval(secs => $2)
WHERE binding LIKE $1 AND payload_until > occurred_at + make_interval(secs => $2)`, scopeHash+":%", retention.Seconds())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BindOwner records durable ownership before ingestion, so a disabled or
// reconfigured runtime can still erase all its previous raw payload copies.
func (s *Store) BindOwner(ctx context.Context, scope Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO sage.lakebaseobs_owners(deployment_id,database_id,scope_hash,policy_binding)
VALUES($1::uuid,$2::uuid,$3,$4)ON CONFLICT DO NOTHING`, scope.DeploymentID, scope.DatabaseID, scope.Hash(), scope.PolicyBinding)
	return err
}

// ActivateOwner is called only after trusted resource verification, before the
// runtime starts. Poll/BindOwner never reactivates a revoked delivery policy.
func (s *Store) ActivateOwner(ctx context.Context, scope Scope, allowPayload bool) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_owners(deployment_id,database_id,scope_hash,policy_binding)
VALUES($1::uuid,$2::uuid,$3,$4)ON CONFLICT DO NOTHING`, scope.DeploymentID, scope.DatabaseID, scope.Hash(), scope.PolicyBinding)
	if err != nil {
		return err
	}
	if err = s.setOwnerPolicy(ctx, tx, scope.DeploymentID, scope.DatabaseID, []string{scope.Hash()}, allowPayload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setOwnerPolicy holds an update lock through revocation and payload erasure.
// An earlier process cannot overwrite a later deployment's disable decision.
func (s *Store) setOwnerPolicy(ctx context.Context, tx pgx.Tx, deployment, database string, allowed []string, allowPayload bool) error {
	if allowed == nil {
		allowed = []string{}
	}
	if err := s.lockOwnerPolicy(ctx, tx, deployment, database); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE sage.lakebaseobs_owner_policy SET allowed_scopes=$3::text[],allow_payload=$4,
policy_epoch=$5,updated_at=now() WHERE deployment_id=$1::uuid AND database_id=$2::uuid`, deployment, database, allowed, allowPayload, s.epoch())
	return err
}

func (s *Store) lockOwnerPolicy(ctx context.Context, tx pgx.Tx, deployment, database string) error {
	_, err := tx.Exec(ctx, `INSERT INTO sage.lakebaseobs_owner_policy(deployment_id,database_id,policy_epoch)
VALUES($1::uuid,$2::uuid,$3)ON CONFLICT DO NOTHING`, deployment, database, s.epoch())
	if err != nil {
		return err
	}
	var epoch time.Time
	if err = tx.QueryRow(ctx, `SELECT policy_epoch FROM sage.lakebaseobs_owner_policy
WHERE deployment_id=$1::uuid AND database_id=$2::uuid FOR UPDATE`, deployment, database).Scan(&epoch); err != nil {
		return err
	}
	if epoch.After(s.epoch()) {
		return ErrStalePolicy
	}
	return nil
}

// A shared policy lock spans the accepting transaction. Cleanup must acquire an
// update lock before revocation, so a stale accepting commit cannot follow it.
// Unregistered legacy fixture bindings retain their prior standalone contract.
func (s *Store) lockDeliveryPolicy(ctx context.Context, tx pgx.Tx, binding string, events []Event) error {
	hash := strings.SplitN(binding, ":", 2)[0]
	var allowed []string
	var payload bool
	err := tx.QueryRow(ctx, `SELECT p.allowed_scopes,p.allow_payload
FROM sage.lakebaseobs_owners o JOIN sage.lakebaseobs_owner_policy p
ON p.deployment_id=o.deployment_id AND p.database_id=o.database_id
WHERE o.scope_hash=$1 FOR SHARE OF p`, hash).Scan(&allowed, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(allowed, hash) {
		return ErrPolicy
	}
	for _, e := range events {
		if e.ScopeHash != hash {
			return ErrScope
		}
		if e.PayloadAllowed && !payload {
			return ErrPolicy
		}
	}
	return nil
}

func validateOwner(deployment, database string) error {
	d, e := uuid.Parse(deployment)
	if e != nil || d == uuid.Nil {
		return ErrScope
	}
	db, e := uuid.Parse(database)
	if e != nil || db == uuid.Nil {
		return ErrScope
	}
	return nil
}

func (s *Store) OwnerScopes(ctx context.Context, deployment, database string) ([]string, error) {
	if err := validateOwner(deployment, database); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT scope_hash FROM sage.lakebaseobs_owners
WHERE deployment_id=$1::uuid AND database_id=$2::uuid ORDER BY scope_hash`, deployment, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// CleanupOwner must run independently of collection and API/credential health.
// Empty allowedScope disables the source. Revocation erases the complete event
// document for earlier payload-enabled epochs; metadata-only events may remain
// for the current scope. Identifiers/ACKs survive to prevent replay resurrection.
func (s *Store) CleanupOwner(ctx context.Context, deployment, database, allowedScope string, allowPayload bool, retention time.Duration, now time.Time) error {
	allowed := []string{}
	if allowedScope != "" {
		allowed = append(allowed, allowedScope)
	}
	return s.CleanupOwnerScopes(ctx, deployment, database, allowed, allowPayload, retention, now)
}

func (s *Store) PolicyOwnerScopes(ctx context.Context, deployment, database, policyBinding string) ([]string, error) {
	if err := validateOwner(deployment, database); err != nil {
		return nil, err
	}
	if len(policyBinding) != 64 {
		return nil, ErrScope
	}
	if _, err := hex.DecodeString(policyBinding); err != nil {
		return nil, ErrScope
	}
	rows, err := s.pool.Query(ctx, `SELECT scope_hash FROM sage.lakebaseobs_owners
WHERE deployment_id=$1::uuid AND database_id=$2::uuid AND policy_binding=$3 ORDER BY scope_hash`, deployment, database, policyBinding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var hash string
		if err = rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

// CleanupOwnerPolicy is the startup/outage path: preserve only scopes already
// verified for this exact trusted configuration, without needing the SDK now.
// An empty policy explicitly disables/revokes the configured source.
func (s *Store) CleanupOwnerPolicy(ctx context.Context, deployment, database, policyBinding string, allowPayload bool, retention time.Duration, now time.Time) error {
	if policyBinding != "" {
		if len(policyBinding) != 64 {
			return ErrScope
		}
		if _, err := hex.DecodeString(policyBinding); err != nil {
			return ErrScope
		}
	}
	return s.cleanupOwner(ctx, deployment, database, nil, &policyBinding, allowPayload, retention, now)
}

func (s *Store) CleanupOwnerScopes(ctx context.Context, deployment, database string, allowedScopes []string, allowPayload bool, retention time.Duration, now time.Time) error {
	return s.cleanupOwner(ctx, deployment, database, allowedScopes, nil, allowPayload, retention, now)
}

func (s *Store) cleanupOwner(ctx context.Context, deployment, database string, allowedScopes []string, policyBinding *string, allowPayload bool, retention time.Duration, now time.Time) error {
	if err := validateOwner(deployment, database); err != nil {
		return err
	}
	if retention <= 0 || retention > SourceRetention {
		return ErrScope
	}
	if allowedScopes == nil {
		allowedScopes = []string{}
	}
	for _, allowedScope := range allowedScopes {
		if len(allowedScope) != 64 {
			return ErrScope
		}
		if _, err := hex.DecodeString(allowedScope); err != nil {
			return ErrScope
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = s.lockOwnerPolicy(ctx, tx, deployment, database); err != nil {
		return err
	}
	if policyBinding != nil && *policyBinding != "" {
		rows, qerr := tx.Query(ctx, `SELECT scope_hash FROM sage.lakebaseobs_owners WHERE deployment_id=$1::uuid
AND database_id=$2::uuid AND policy_binding=$3 ORDER BY scope_hash`, deployment, database, *policyBinding)
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var hash string
			if qerr = rows.Scan(&hash); qerr != nil {
				rows.Close()
				return qerr
			}
			allowedScopes = append(allowedScopes, hash)
		}
		qerr = rows.Err()
		rows.Close()
		if qerr != nil {
			return qerr
		}
	}
	if err = s.setOwnerPolicy(ctx, tx, deployment, database, allowedScopes, allowPayload); err != nil {
		return err
	}
	// Ownership is a join predicate, not an account-wide prefix chosen by the
	// caller. A database cannot erase another registered database's rows.
	if _, err = tx.Exec(ctx, `UPDATE sage.lakebaseobs_inbox i
SET payload_until=LEAST(i.payload_until,i.occurred_at+make_interval(secs=>$3))
WHERE i.payload_until > i.occurred_at+make_interval(secs=>$3)
AND EXISTS(SELECT 1 FROM sage.lakebaseobs_owners o WHERE o.deployment_id=$1::uuid
AND o.database_id=$2::uuid AND i.binding LIKE o.scope_hash||':%')`, deployment, database, retention.Seconds()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE sage.lakebaseobs_inbox i SET event=NULL
WHERE event IS NOT NULL AND EXISTS(SELECT 1 FROM sage.lakebaseobs_owners o
WHERE o.deployment_id=$1::uuid AND o.database_id=$2::uuid AND i.binding LIKE o.scope_hash||':%'
AND(NOT(o.scope_hash=ANY($3::text[])) OR (NOT $4 AND COALESCE((i.event->>'payload_allowed')::boolean,false)) OR i.payload_until<=$5))`, deployment, database, allowedScopes, allowPayload, now); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sage.lakebaseobs_inbox i WHERE identity_until<=$3
AND EXISTS(SELECT 1 FROM sage.lakebaseobs_owners o WHERE o.deployment_id=$1::uuid
AND o.database_id=$2::uuid AND i.binding LIKE o.scope_hash||':%')`, deployment, database, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Purge erases payloads at event-time expiry but preserves identities/ACKs for
// the entire seven-day replay horizon plus clock/delivery margin. Small committed
// batches release the shared control pool between batches and make progress even
// when the backlog exceeds one operation's budget. ErrRetentionPending requests
// another scheduled pass; collection and the independent erasure worker both
// retry. Pending, History, delivery and evidence refuse expired payloads even
// while physical erasure remains pending. No locked rows are silently skipped.
func (s *Store) Purge(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, storeOperationTimeout)
	defer cancel()
	for {
		more, err := s.purgeBatch(ctx, now)
		if err != nil {
			return errors.Join(ErrRetentionPending, err)
		}
		if !more {
			return nil
		}
	}
}

func (s *Store) purgeBatch(ctx context.Context, now time.Time) (bool, error) {
	tx, err := beginStoreOperation(ctx, s.pool, false)
	if err != nil {
		return false, err
	}
	defer rollbackStoreOperation(tx)
	erased, err := tx.Exec(ctx, `WITH expired AS (
 SELECT binding,event_id FROM sage.lakebaseobs_inbox
 WHERE event IS NOT NULL AND payload_until<=$1
 ORDER BY payload_until,binding,event_id LIMIT $2 FOR UPDATE)
UPDATE sage.lakebaseobs_inbox i SET event=NULL FROM expired e
WHERE i.binding=e.binding AND i.event_id=e.event_id`, now, purgeBatchSize)
	if err != nil {
		return false, err
	}
	deleted, err := tx.Exec(ctx, `WITH expired AS (
 SELECT binding,event_id FROM sage.lakebaseobs_inbox WHERE identity_until<=$1
 ORDER BY identity_until,binding,event_id LIMIT $2 FOR UPDATE)
DELETE FROM sage.lakebaseobs_inbox i USING expired e
WHERE i.binding=e.binding AND i.event_id=e.event_id`, now, purgeBatchSize)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return erased.RowsAffected() == purgeBatchSize || deleted.RowsAffected() == purgeBatchSize, nil
}
