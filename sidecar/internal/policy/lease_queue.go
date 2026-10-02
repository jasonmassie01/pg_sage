package policy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Serialize modes: what a change lease conflict does (decision D1).
const (
	// SerializePark parks the later request; it retries on its next cycle.
	SerializePark = "park"
	// SerializeQueue makes the later request wait its FIFO turn for the
	// object in a durable, bounded queue (sage.lease_queue).
	SerializeQueue = "queue"
)

// Queue outcomes. Each is a lease conflict, so a caller that parks on a
// conflict parks on these too.
var (
	ErrLeaseQueueTimeout   = fmt.Errorf("%w: lease queue wait timed out", ErrLeaseConflict)
	ErrLeaseQueueFull      = fmt.Errorf("%w: lease queue is full", ErrLeaseConflict)
	ErrLeaseQueueDuplicate = fmt.Errorf("%w: an identical request is already queued",
		ErrLeaseConflict)
)

// LeaseQueueConfig bounds the lease queue.
type LeaseQueueConfig struct {
	// MaxWait bounds a self-initiated request's wait; past it, it parks.
	MaxWait time.Duration
	// OperatorMaxWait bounds an operator action's wait (a person is waiting
	// on the request); past it, the action is refused.
	OperatorMaxWait time.Duration
	// MaxDepthPerTarget and MaxDepth bound the waiting entries sharing an
	// object and in the whole database; a request beyond them is refused.
	MaxDepthPerTarget int
	MaxDepth          int
	// PollInterval is how often a waiter checks its turn (and heartbeats).
	PollInterval time.Duration
	// StaleAfter is how long a waiter may miss heartbeats before a
	// restarted sidecar resubmitting the same request resumes its entry.
	StaleAfter time.Duration
}

// DefaultLeaseQueueConfig is the shipped bound: two minutes for pg_sage's
// own actions (one cycle's patience), thirty seconds for an operator.
func DefaultLeaseQueueConfig() LeaseQueueConfig {
	return LeaseQueueConfig{
		MaxWait: 2 * time.Minute, OperatorMaxWait: 30 * time.Second,
		MaxDepthPerTarget: 8, MaxDepth: 64,
		PollInterval: 500 * time.Millisecond, StaleAfter: 5 * time.Second,
	}
}

// LeaseQueue grants change leases in FIFO order per object. Entries are
// durable: a waiter that dies keeps its place until its deadline, and the
// same request resubmitted after a restart resumes it.
type LeaseQueue struct {
	pool       *pgxpool.Pool
	databaseID *int
	cfg        LeaseQueueConfig
	instance   string
}

// NewLeaseQueue returns the queue of databaseID (nil for a standalone
// database). Zero config fields take the defaults; instance identifies
// this process (ProcessInstance in production).
func NewLeaseQueue(
	pool *pgxpool.Pool, databaseID *int, cfg LeaseQueueConfig, instance string,
) *LeaseQueue {
	return &LeaseQueue{pool: pool, databaseID: databaseID,
		cfg: withQueueDefaults(cfg), instance: instance}
}

func withQueueDefaults(cfg LeaseQueueConfig) LeaseQueueConfig {
	defaults := DefaultLeaseQueueConfig()
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = defaults.MaxWait
	}
	if cfg.OperatorMaxWait <= 0 {
		cfg.OperatorMaxWait = defaults.OperatorMaxWait
	}
	if cfg.MaxDepthPerTarget <= 0 {
		cfg.MaxDepthPerTarget = defaults.MaxDepthPerTarget
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = defaults.MaxDepth
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaults.PollInterval
	}
	if cfg.StaleAfter <= 0 {
		cfg.StaleAfter = defaults.StaleAfter
	}
	return cfg
}

// QueuedLeaseRequest is one request for a typed lease through the queue.
type QueuedLeaseRequest struct {
	Manager *PostgresLeaseManager
	// Kind (finding, custodian, operator, retention), Intent and the
	// targets identify the request across a restart.
	Kind    string
	Actor   string
	Intent  string
	Targets []TypedTarget
	// Operator marks a person's action: it waits at most OperatorMaxWait.
	Operator bool
	// MaxWait bounds this request's wait; zero uses the queue's bound.
	MaxWait time.Duration
}

// Acquire takes the lease at once when the objects are free and nobody is
// waiting for them; otherwise it queues (or resumes its entry) and waits
// its turn. It returns ErrLeaseQueueFull, ErrLeaseQueueDuplicate or
// ErrLeaseQueueTimeout (all lease conflicts), or the caller's context
// error when the caller gave up.
func (q *LeaseQueue) Acquire(ctx context.Context, req QueuedLeaseRequest) (LeaseID, error) {
	if err := q.validate(req); err != nil {
		return "", err
	}
	keys := TypedLeaseKeys(req.Targets)
	requestKey := leaseRequestKey(req.Kind, req.Intent, keys)
	if err := q.sweep(ctx); err != nil {
		return "", err
	}
	id, err := q.resume(ctx, req, requestKey)
	if err != nil {
		return "", err
	}
	if id == 0 {
		leaseID, granted, err := q.tryImmediate(ctx, req, keys)
		if granted || err != nil {
			return leaseID, err
		}
		if id, err = q.enqueue(ctx, req, requestKey, keys); err != nil {
			return "", err
		}
	}
	return q.wait(ctx, req, id)
}

func (q *LeaseQueue) validate(req QueuedLeaseRequest) error {
	switch {
	case q == nil || q.pool == nil:
		return errors.New("lease queue database is unavailable")
	case req.Manager == nil || req.Manager.decisionID <= 0:
		return errors.New("lease queue request needs a lease manager with a decision")
	case len(req.Targets) == 0:
		return errors.New("lease queue request has no target objects")
	case strings.TrimSpace(req.Kind) == "" || strings.TrimSpace(req.Actor) == "" ||
		strings.TrimSpace(req.Intent) == "":
		return errors.New("lease queue request needs a kind, actor and intent")
	case req.MaxWait < 0:
		return errors.New("lease queue wait bound must not be negative")
	}
	return nil
}

func (q *LeaseQueue) maxWait(req QueuedLeaseRequest) time.Duration {
	switch {
	case req.MaxWait > 0:
		return req.MaxWait
	case req.Operator:
		return q.cfg.OperatorMaxWait
	}
	return q.cfg.MaxWait
}

// tryImmediate takes the lease when nobody waits for the objects. A
// conflict is not an error here: the request queues.
func (q *LeaseQueue) tryImmediate(
	ctx context.Context, req QueuedLeaseRequest, keys []string,
) (LeaseID, bool, error) {
	var waiting bool
	err := q.pool.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
		FROM sage.lease_queue
		WHERE state='waiting' AND deadline_at > now() AND object_keys && $1
		  AND COALESCE(database_id, 0) = COALESCE($2::bigint, 0))`,
		keys, q.databaseID).Scan(&waiting)
	if err != nil {
		return "", false, fmt.Errorf("read lease queue: %w", err)
	}
	if waiting {
		return "", false, nil
	}
	leaseID, err := req.Manager.AcquireTyped(ctx, req.Actor, req.Targets, req.Intent)
	if retryableLeaseError(err) {
		return "", false, nil
	}
	return leaseID, err == nil, err
}

func retryableLeaseError(err error) bool {
	return errors.Is(err, ErrLeaseConflict) || errors.Is(err, ErrLeaseBusy)
}

// leaseRequestKey identifies a request across a restart.
func leaseRequestKey(kind, intent string, keys []string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + intent + "\x00" +
		strings.Join(keys, "\x00")))
	return hex.EncodeToString(digest[:])
}

// sweep resolves entries whose deadline passed, so an abandoned entry
// never blocks the queue beyond its bound.
func (q *LeaseQueue) sweep(ctx context.Context) error {
	_, err := q.pool.Exec(ctx, `/* pg_sage */ UPDATE sage.lease_queue
		SET state='timed_out', resolved_at=now()
		WHERE state='waiting' AND deadline_at <= now()
		  AND COALESCE(database_id, 0) = COALESCE($1::bigint, 0)`, q.databaseID)
	if err != nil {
		return fmt.Errorf("expire lease queue entries: %w", err)
	}
	return nil
}

// resume takes over this request's entry left by a previous process (its
// heartbeat stopped), keeping its place and its original deadline.
func (q *LeaseQueue) resume(
	ctx context.Context, req QueuedLeaseRequest, requestKey string,
) (int64, error) {
	var id int64
	err := q.pool.QueryRow(ctx, `/* pg_sage */ UPDATE sage.lease_queue
		SET instance=$3, heartbeat_at=now(), decision_id=$4, actor=$5,
		    resumed_count=resumed_count + 1
		WHERE COALESCE(database_id, 0) = COALESCE($1::bigint, 0) AND request_key=$2
		  AND state='waiting' AND deadline_at > now() AND instance <> $3
		  AND heartbeat_at < now() - make_interval(secs => $6)
		RETURNING id`, q.databaseID, requestKey, q.instance, req.Manager.decisionID,
		req.Actor, q.cfg.StaleAfter.Seconds()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("resume lease queue entry: %w", err)
	}
	return id, nil
}

// processInstance identifies this sidecar process in the lease queue.
var processInstance = "process-" + rand.Text()

// ProcessInstance identifies this sidecar process in the lease queue.
func ProcessInstance() string { return processInstance }
