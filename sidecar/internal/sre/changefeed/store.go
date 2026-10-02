package changefeed

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
)

// List bounds.
const (
	DefaultList = 100
	MaxList     = 500
)

// Store persists the feed in sage.sre_change_events of the coordination
// database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore binds a store to the coordination pool.
func NewStore(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: change feed store needs a pool", sre.ErrInvalidRequest)
	}
	return &Store{pool: pool}, nil
}

// Key identifies the database behind the store: stores sharing a pool
// share their rows.
func (s *Store) Key() *pgxpool.Pool { return s.pool }

// Filter bounds a list: events that occurred in [Since, Until] (a zero
// Until is now), newest first, at most Limit (default 100, max 500).
type Filter struct {
	Since time.Time
	Until time.Time
	Limit int
}

const eventColumns = `id::text, source, event_id, kind, database_id IS NOT NULL,
	COALESCE(service, ''), summary, COALESCE(link, ''), objects::text, occurred_at,
	received_at, signature_status, change_hash`

// Record stores an event once per (source, event id). A replay returns
// the stored event with created false; the same id with other content
// is ErrConflict. scoped binds the event to the scope's database;
// otherwise it is deployment-wide.
func (s *Store) Record(ctx context.Context, scope sre.Scope, scoped bool,
	e Event) (Event, bool, error) {
	if err := scope.Validate(); err != nil {
		return Event{}, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	hash, err := hex.DecodeString(e.Hash)
	if err != nil || len(hash) != 32 {
		return Event{}, false, fmt.Errorf("%w: event hash", ErrInvalid)
	}
	objects, err := json.Marshal(e.Objects)
	if err != nil {
		return Event{}, false, fmt.Errorf("%w: objects: %v", ErrInvalid, err)
	}
	var db *string
	if scoped {
		v := string(scope.DatabaseID)
		db = &v
	}
	row := s.pool.QueryRow(ctx, `INSERT INTO sage.sre_change_events
		(deployment_id, id, database_id, source, event_id, kind, service, summary, link,
		 objects, occurred_at, received_at, signature_status, change_hash)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, NULLIF($9, ''), $10::jsonb,
		        $11, $12, $13, $14)
		ON CONFLICT (deployment_id, source, event_id) DO NOTHING
		RETURNING `+eventColumns, string(scope.DeploymentID), string(sre.NewUUID()), db,
		e.Source, e.EventID, string(e.Kind), e.Service, e.Summary, e.Link, string(objects),
		e.OccurredAt, e.ReceivedAt, e.Signature, hash)
	stored, err := scanEvent(row)
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Event{}, false, fmt.Errorf("record change event: %w", err)
	}
	return s.existing(ctx, scope, e)
}

// existing returns the stored event of a replayed submission.
func (s *Store) existing(ctx context.Context, scope sre.Scope, e Event) (Event, bool, error) {
	stored, err := scanEvent(s.pool.QueryRow(ctx, `SELECT `+eventColumns+`
		FROM sage.sre_change_events
		WHERE deployment_id = $1 AND source = $2 AND event_id = $3`,
		string(scope.DeploymentID), e.Source, e.EventID))
	if err != nil {
		return Event{}, false, fmt.Errorf("read replayed change event: %w", err)
	}
	if stored.Hash != e.Hash {
		return stored, false, fmt.Errorf("%w: %s/%s", ErrConflict, e.Source, e.EventID)
	}
	return stored, false, nil
}

// List reads the scope's events (its database's and deployment-wide
// ones) that occurred in the filter's window, newest first.
func (s *Store) List(ctx context.Context, scope sre.Scope, f Filter) ([]Event, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	limit := f.Limit
	switch {
	case limit < 0 || limit > MaxList:
		return nil, fmt.Errorf("%w: limit must be 1-%d", ErrInvalid, MaxList)
	case limit == 0:
		limit = DefaultList
	}
	until := f.Until
	if until.IsZero() {
		until = time.Now().Add(maxFuture)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+eventColumns+`
		FROM sage.sre_change_events
		WHERE deployment_id = $1 AND (database_id = $2 OR database_id IS NULL)
		  AND occurred_at >= $3 AND occurred_at <= $4
		ORDER BY occurred_at DESC, received_at DESC, id
		LIMIT $5`, string(scope.DeploymentID), string(scope.DatabaseID), f.Since, until, limit)
	if err != nil {
		return nil, fmt.Errorf("list change events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan change event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Purge deletes the deployment's events received before the cutoff.
func (s *Store) Purge(ctx context.Context, deployment sre.UUID, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sage.sre_change_events
		WHERE deployment_id = $1 AND received_at < $2`, string(deployment), before)
	if err != nil {
		return 0, fmt.Errorf("purge change events: %w", err)
	}
	return tag.RowsAffected(), nil
}

func scanEvent(row pgx.Row) (Event, error) {
	var e Event
	var id, kind, objects string
	var hash []byte
	err := row.Scan(&id, &e.Source, &e.EventID, &kind, &e.Scoped, &e.Service, &e.Summary,
		&e.Link, &objects, &e.OccurredAt, &e.ReceivedAt, &e.Signature, &hash)
	if err != nil {
		return Event{}, err
	}
	e.ID, e.Kind, e.Hash = sre.UUID(id), Kind(kind), hex.EncodeToString(hash)
	e.OccurredAt, e.ReceivedAt = e.OccurredAt.UTC(), e.ReceivedAt.UTC()
	if err := json.Unmarshal([]byte(objects), &e.Objects); err != nil {
		return Event{}, fmt.Errorf("objects: %w", err)
	}
	return e, nil
}
