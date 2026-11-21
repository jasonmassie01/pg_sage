package mcp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RequestStatusReader reads a held item from the table its own page reads,
// on the database's pool (where pg_sage keeps its tables): one bounded,
// parameterized single-row SELECT per read. It never writes.

// statusReadTimeout bounds one status read.
const statusReadTimeout = 5 * time.Second

// StatusPools is a database's pool; an error for a database that is not
// monitored or not connected.
type StatusPools func(ctx context.Context, database string) (*pgxpool.Pool, error)

// RequestStatusReader serves RequestStatusBackend from the stores.
type RequestStatusReader struct{ pools StatusPools }

// NewRequestStatusReader reads through pools.
func NewRequestStatusReader(pools StatusPools) *RequestStatusReader {
	return &RequestStatusReader{pools: pools}
}

const (
	queueStatusSQL = `/* pg_sage mcp_request_status queue v1 */ SELECT status,
  COALESCE(verification_status, ''), decided_by, COALESCE(principal_id, '')
FROM sage.action_queue WHERE id = $1`
	planStatusSQL = `/* pg_sage mcp_request_status plan v1 */ SELECT status,
  CASE WHEN origin = 'agent' THEN origin_ref ELSE '' END, created_by
FROM sage.plan WHERE id = $1`
	grantStatusSQL = `/* pg_sage mcp_request_status grant v1 */ SELECT status,
  decided_by IS NOT NULL, principal_id FROM sage.guard_grant_requests WHERE id = $1`
	policyStatusSQL = `/* pg_sage mcp_request_status policy v1 */ SELECT status,
  ratified_at IS NOT NULL, COALESCE(proposed_principal_id, ''), proposed_by
FROM sage.policy WHERE id = $1`
	decisionStatusSQL = `/* pg_sage mcp_request_status decision v1 */ SELECT verdict,
  resolved_at IS NOT NULL, action_log_id IS NOT NULL, queue_id, COALESCE(principal_id, '')
FROM sage.decision WHERE evidence_id = $1 ORDER BY id DESC LIMIT 1`
)

// RequestStatus reads q's item.
func (r *RequestStatusReader) RequestStatus(ctx context.Context, q RequestQuery) (
	RequestRecord, error) {
	if r == nil || r.pools == nil {
		return RequestRecord{}, ErrProductionDependencyUnavailable
	}
	pool, err := r.pools(ctx, q.Database)
	if err != nil {
		return RequestRecord{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, statusReadTimeout)
	defer cancel()
	record, err := readStatus(ctx, pool, q)
	if errors.Is(err, pgx.ErrNoRows) {
		return RequestRecord{}, fmt.Errorf("%s: %w", handleOf(q), ErrRequestNotFound)
	}
	if err != nil {
		return RequestRecord{}, fmt.Errorf("read status of %s on %s: %w", handleOf(q),
			q.Database, err)
	}
	return record, nil
}

func readStatus(ctx context.Context, pool *pgxpool.Pool, q RequestQuery) (RequestRecord,
	error) {
	if q.Kind == "change" {
		return readDecision(ctx, pool, q.ID)
	}
	id, err := strconv.ParseInt(q.ID, 10, 64)
	if err != nil {
		return RequestRecord{}, pgx.ErrNoRows
	}
	switch q.Kind {
	case "change_approval":
		return readQueue(ctx, pool, id)
	case "plan":
		var status, principal, actor string
		err = pool.QueryRow(ctx, planStatusSQL, id).Scan(&status, &principal, &actor)
		return owned(planState(status), principal, actor), err
	case "agent_request":
		var status, principal string
		var decided bool
		err = pool.QueryRow(ctx, grantStatusSQL, id).Scan(&status, &decided, &principal)
		return owned(grantState(status, decided), principal, ""), err
	case "policy_exception":
		var status, principal, actor string
		var ratified bool
		err = pool.QueryRow(ctx, policyStatusSQL, id).Scan(&status, &ratified, &principal,
			&actor)
		return owned(policyState(status, ratified), principal, actor), err
	}
	return RequestRecord{}, pgx.ErrNoRows
}

func readQueue(ctx context.Context, pool *pgxpool.Pool, id int64) (RequestRecord, error) {
	var status, verification, principal string
	var decidedBy *int
	err := pool.QueryRow(ctx, queueStatusSQL, id).Scan(&status, &verification, &decidedBy,
		&principal)
	return owned(queueState(status, verification, decidedBy), principal, ""), err
}

// readDecision: a withheld decision that queued an approval follows that
// queue row; the decision's principal still owns it.
func readDecision(ctx context.Context, pool *pgxpool.Pool, evidenceID string) (
	RequestRecord, error) {
	var verdict, principal string
	var resolved, executed bool
	var queueID *int64
	if err := pool.QueryRow(ctx, decisionStatusSQL, evidenceID).Scan(&verdict, &resolved,
		&executed, &queueID, &principal); err != nil {
		return RequestRecord{}, err
	}
	if queueID == nil {
		return owned(decisionState(verdict, resolved, executed), principal, ""), nil
	}
	record, err := readQueue(ctx, pool, *queueID)
	if err != nil {
		return RequestRecord{}, err
	}
	return owned(record, principal, ""), nil
}

func owned(record RequestRecord, principal, actor string) RequestRecord {
	record.OwnerPrincipalID, record.OwnerActor = principal, actor
	return record
}
