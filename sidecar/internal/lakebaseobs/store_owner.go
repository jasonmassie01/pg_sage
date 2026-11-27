package lakebaseobs

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const insertOwnerSQL = `INSERT INTO
sage.lakebaseobs_owners(deployment_id,database_id,scope_hash,policy_binding)
VALUES($1::uuid,$2::uuid,$3,$4)ON CONFLICT DO NOTHING`

// BindOwner records durable ownership before ingestion, so a disabled or
// reconfigured runtime can still erase all its previous raw payload copies.
func (s *Store) BindOwner(ctx context.Context, scope Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, insertOwnerSQL,
		scope.DeploymentID, scope.DatabaseID, scope.Hash(), scope.PolicyBinding)
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
	_, err = tx.Exec(ctx, insertOwnerSQL,
		scope.DeploymentID, scope.DatabaseID, scope.Hash(), scope.PolicyBinding)
	if err != nil {
		return err
	}
	err = s.setOwnerPolicy(ctx, tx, scope.DeploymentID, scope.DatabaseID,
		[]string{scope.Hash()}, allowPayload)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setOwnerPolicy holds an update lock through revocation and payload erasure.
// An earlier process cannot overwrite a later deployment's disable decision.
func (s *Store) setOwnerPolicy(ctx context.Context, tx pgx.Tx, deployment, database string,
	allowed []string, allowPayload bool) error {
	if allowed == nil {
		allowed = []string{}
	}
	if err := s.lockOwnerPolicy(ctx, tx, deployment, database); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE sage.lakebaseobs_owner_policy
SET allowed_scopes=$3::text[],allow_payload=$4,
policy_epoch=$5,updated_at=now() WHERE deployment_id=$1::uuid AND database_id=$2::uuid`,
		deployment, database, allowed, allowPayload, s.epoch())
	return err
}

func (s *Store) lockOwnerPolicy(ctx context.Context, tx pgx.Tx, deployment, database string) error {
	_, err := tx.Exec(ctx, `INSERT INTO
sage.lakebaseobs_owner_policy(deployment_id,database_id,policy_epoch)
VALUES($1::uuid,$2::uuid,$3)ON CONFLICT DO NOTHING`, deployment, database, s.epoch())
	if err != nil {
		return err
	}
	var epoch time.Time
	if err = tx.QueryRow(ctx, `SELECT policy_epoch FROM sage.lakebaseobs_owner_policy
WHERE deployment_id=$1::uuid AND database_id=$2::uuid FOR UPDATE`,
		deployment, database).Scan(&epoch); err != nil {
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
func (s *Store) lockDeliveryPolicy(ctx context.Context, tx pgx.Tx, binding string,
	events []Event) error {
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
WHERE deployment_id=$1::uuid AND database_id=$2::uuid ORDER BY scope_hash`,
		deployment, database)
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
func (s *Store) CleanupOwner(ctx context.Context, deployment, database, allowedScope string,
	allowPayload bool, retention time.Duration, now time.Time) error {
	allowed := []string{}
	if allowedScope != "" {
		allowed = append(allowed, allowedScope)
	}
	return s.CleanupOwnerScopes(ctx, deployment, database, allowed, allowPayload, retention, now)
}

func (s *Store) PolicyOwnerScopes(ctx context.Context, deployment, database,
	policyBinding string) ([]string, error) {
	if err := validateOwner(deployment, database); err != nil {
		return nil, err
	}
	if len(policyBinding) != 64 {
		return nil, ErrScope
	}
	if _, err := hex.DecodeString(policyBinding); err != nil {
		return nil, ErrScope
	}
	return selectPolicyScopes(ctx, s.pool, deployment, database, policyBinding)
}

// CleanupOwnerPolicy is the startup/outage path: preserve only scopes already
// verified for this exact trusted configuration, without needing the SDK now.
// An empty policy explicitly disables/revokes the configured source.
func (s *Store) CleanupOwnerPolicy(ctx context.Context, deployment, database, policyBinding string,
	allowPayload bool, retention time.Duration, now time.Time) error {
	if policyBinding != "" {
		if len(policyBinding) != 64 {
			return ErrScope
		}
		if _, err := hex.DecodeString(policyBinding); err != nil {
			return ErrScope
		}
	}
	return s.cleanupOwner(ctx, deployment, database, nil, &policyBinding, allowPayload,
		retention, now)
}

func (s *Store) CleanupOwnerScopes(ctx context.Context, deployment, database string,
	allowedScopes []string, allowPayload bool, retention time.Duration, now time.Time) error {
	return s.cleanupOwner(ctx, deployment, database, allowedScopes, nil, allowPayload,
		retention, now)
}

func (s *Store) cleanupOwner(ctx context.Context, deployment, database string,
	allowedScopes []string, policyBinding *string, allowPayload bool,
	retention time.Duration, now time.Time) error {
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
		bound, qerr := selectPolicyScopes(ctx, tx, deployment, database, *policyBinding)
		if qerr != nil {
			return qerr
		}
		allowedScopes = append(allowedScopes, bound...)
	}
	err = s.setOwnerPolicy(ctx, tx, deployment, database, allowedScopes, allowPayload)
	if err != nil {
		return err
	}
	err = eraseOwnerScopes(ctx, tx, deployment, database, allowedScopes, allowPayload,
		retention, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// rowQuerier is satisfied by both the pool and a transaction.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// selectPolicyScopes lists the scopes already verified under one trusted policy
// binding. Inside a cleanup transaction they stay allowed by that cleanup.
func selectPolicyScopes(ctx context.Context, q rowQuerier, deployment, database,
	policyBinding string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT scope_hash FROM sage.lakebaseobs_owners
WHERE deployment_id=$1::uuid AND database_id=$2::uuid AND policy_binding=$3
ORDER BY scope_hash`, deployment, database, policyBinding)
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

// eraseOwnerScopes tightens retention, erases payloads that the new policy no
// longer allows or that expired, and deletes expired identities, in that order.
// Ownership is a join predicate, not an account-wide prefix chosen by the
// caller. A database cannot erase another registered database's rows.
func eraseOwnerScopes(ctx context.Context, tx pgx.Tx, deployment, database string,
	allowedScopes []string, allowPayload bool, retention time.Duration, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE sage.lakebaseobs_inbox i
SET payload_until=LEAST(i.payload_until,i.occurred_at+make_interval(secs=>$3))
WHERE i.payload_until > i.occurred_at+make_interval(secs=>$3)
AND EXISTS(SELECT 1 FROM sage.lakebaseobs_owners o WHERE o.deployment_id=$1::uuid
AND o.database_id=$2::uuid AND i.binding LIKE o.scope_hash||':%')`,
		deployment, database, retention.Seconds()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sage.lakebaseobs_inbox i SET event=NULL
WHERE event IS NOT NULL AND EXISTS(SELECT 1 FROM sage.lakebaseobs_owners o
WHERE o.deployment_id=$1::uuid AND o.database_id=$2::uuid
AND i.binding LIKE o.scope_hash||':%'
AND(NOT(o.scope_hash=ANY($3::text[]))
OR (NOT $4 AND COALESCE((i.event->>'payload_allowed')::boolean,false)) OR i.payload_until<=$5))`,
		deployment, database, allowedScopes, allowPayload, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM sage.lakebaseobs_inbox i WHERE identity_until<=$3
AND EXISTS(SELECT 1 FROM sage.lakebaseobs_owners o WHERE o.deployment_id=$1::uuid
AND o.database_id=$2::uuid AND i.binding LIKE o.scope_hash||':%')`,
		deployment, database, now)
	return err
}
