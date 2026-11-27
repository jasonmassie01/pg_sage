package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store keeps the queue in sage.review_trigger of the reviewed database's
// own sage schema. Times a caller passes (fired_at, lease_until, now) are
// the caller's clock, so a scheduler's lease math is one clock.
type Store struct{ pool *pgxpool.Pool }

// NewStore returns a store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const maxKeyLen = 300

func (s *Store) usable() error {
	if s == nil || s.pool == nil {
		return ErrNoStore
	}
	return nil
}

func validate(t Trigger) error {
	switch {
	case !t.Kind.Valid():
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, t.Kind)
	case t.Database == "" || len(t.Database) > 200:
		return fmt.Errorf("%w: database %q", ErrInvalid, t.Database)
	case t.Key == "" || len(t.Key) > maxKeyLen:
		return fmt.Errorf("%w: dedupe key of %d bytes (1-%d)", ErrInvalid, len(t.Key),
			maxKeyLen)
	}
	return nil
}

// Fire stores t unless a trigger of its database, kind and key exists
// (fired false, id 0). A zero FiredAt is the server's now(). Every error
// names the kind and the database and is counted in FireErrors.
func (s *Store) Fire(ctx context.Context, t Trigger) (int64, bool, error) {
	id, fired, err := s.fire(ctx, t)
	if err != nil {
		countFireError(t.Kind)
		return 0, false, fmt.Errorf("review trigger %s for %s: %w", t.Kind, t.Database, err)
	}
	return id, fired, nil
}

func (s *Store) fire(ctx context.Context, t Trigger) (int64, bool, error) {
	if err := validate(t); err != nil {
		return 0, false, err
	}
	if err := s.usable(); err != nil {
		return 0, false, err
	}
	detail := t.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return 0, false, fmt.Errorf("%w: detail: %v", ErrInvalid, err)
	}
	var at *time.Time
	if !t.FiredAt.IsZero() {
		at = &t.FiredAt
	}
	var id int64
	err = s.pool.QueryRow(ctx, `/* pg_sage review trigger */
		INSERT INTO sage.review_trigger (database_name, kind, dedupe_key, detail, fired_at)
		VALUES ($1, $2, $3, $4::jsonb, COALESCE($5::timestamptz, now()))
		ON CONFLICT (database_name, kind, dedupe_key) DO NOTHING RETURNING id`,
		t.Database, string(t.Kind), t.Key, string(raw), at).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert into sage.review_trigger: %w", err)
	}
	return id, true, nil
}

const triggerCols = `id, database_name, kind, dedupe_key, detail, fired_at, state,
	COALESCE(run_id, 0), COALESCE(coalesced_into, 0)`

func scanTriggers(rows pgx.Rows) ([]Trigger, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Trigger, error) {
		var t Trigger
		var kind, state string
		var raw []byte
		err := row.Scan(&t.ID, &t.Database, &kind, &t.Key, &raw, &t.FiredAt, &state,
			&t.RunID, &t.CoalescedInto)
		if err != nil {
			return t, err
		}
		t.Kind, t.State, t.FiredAt = Kind(kind), State(state), t.FiredAt.UTC()
		return t, json.Unmarshal(raw, &t.Detail)
	})
}

func (s *Store) query(ctx context.Context, what, sql string, args ...any) ([]Trigger, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	out, err := scanTriggers(rows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// Pending is database's fired triggers, oldest first.
func (s *Store) Pending(ctx context.Context, database string) ([]Trigger, error) {
	return s.query(ctx, "read pending sage.review_trigger of "+database,
		`SELECT `+triggerCols+` FROM sage.review_trigger
		  WHERE database_name = $1 AND state = 'fired' ORDER BY fired_at, id`, database)
}

// WhyNow is the triggers run consumed or coalesced, oldest first.
func (s *Store) WhyNow(ctx context.Context, runID int64) ([]Trigger, error) {
	return s.query(ctx, fmt.Sprintf("read the triggers of review run %d", runID),
		`SELECT `+triggerCols+` FROM sage.review_trigger
		  WHERE run_id = $1 AND state IN ('consumed', 'coalesced') ORDER BY fired_at, id`,
		runID)
}

// Claim takes the fired triggers of ids for owner until the lease ends,
// skipping rows another claimer has locked, and returns those it took.
func (s *Store) Claim(ctx context.Context, database string, ids []int64, owner string,
	until time.Time) ([]Trigger, error) {
	return s.query(ctx, "claim sage.review_trigger of "+database,
		`UPDATE sage.review_trigger t SET state = 'claimed', claimed_by = $3,
		        lease_until = $4
		  WHERE t.id IN (SELECT id FROM sage.review_trigger
		                  WHERE database_name = $1 AND id = ANY($2) AND state = 'fired'
		                  ORDER BY id FOR UPDATE SKIP LOCKED)
		 RETURNING `+qualified, database, ids, owner, until)
}

var qualified = `t.id, t.database_name, t.kind, t.dedupe_key, t.detail, t.fired_at,
	t.state, COALESCE(t.run_id, 0), COALESCE(t.coalesced_into, 0)`

// exec runs a change that must touch exactly want rows (ErrLeaseLost when
// fewer did, rolled back).
func (s *Store) exec(ctx context.Context, what string, want int, sql string,
	args ...any) error {
	if err := s.usable(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if int(tag.RowsAffected()) != want {
			return fmt.Errorf("%w: %s touched %d of %d triggers", ErrLeaseLost, what,
				tag.RowsAffected(), want)
		}
		return nil
	})
}

// Attach records the run owner's claim started, while it still holds it.
func (s *Store) Attach(ctx context.Context, ids []int64, runID int64, owner string) error {
	return s.exec(ctx, fmt.Sprintf("attach review run %d", runID), len(ids),
		`UPDATE sage.review_trigger SET run_id = $2
		  WHERE id = ANY($1) AND state = 'claimed' AND claimed_by = $3`, ids, runID, owner)
}

// Consume marks the triggers done by runID: the first of ids consumed,
// the rest coalesced into it. A trigger is consumable while claimed, or
// fired again after its lease with this run attached.
func (s *Store) Consume(ctx context.Context, ids []int64, runID int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.exec(ctx, fmt.Sprintf("consume into review run %d", runID), len(ids),
		`UPDATE sage.review_trigger SET
		        state = CASE WHEN id = $2 THEN 'consumed' ELSE 'coalesced' END,
		        coalesced_into = CASE WHEN id = $2 THEN NULL ELSE $2 END,
		        run_id = $3, done_at = now(), claimed_by = NULL, lease_until = NULL
		  WHERE id = ANY($1) AND state IN ('claimed', 'fired')
		    AND (run_id = $3 OR (state = 'claimed' AND run_id IS NULL))`,
		ids, ids[0], runID)
}

// Release returns owner's claims to fired, forgetting any attached run.
func (s *Store) Release(ctx context.Context, ids []int64, owner string) error {
	return s.exec(ctx, "release sage.review_trigger claims", len(ids),
		`UPDATE sage.review_trigger SET state = 'fired', claimed_by = NULL,
		        lease_until = NULL, run_id = NULL
		  WHERE id = ANY($1) AND state = 'claimed' AND claimed_by = $2`, ids, owner)
}

// Detach forgets runID on fired triggers whose run did not finish, so
// they start a new run.
func (s *Store) Detach(ctx context.Context, ids []int64, runID int64) error {
	return s.exec(ctx, fmt.Sprintf("detach failed review run %d", runID), len(ids),
		`UPDATE sage.review_trigger SET run_id = NULL
		  WHERE id = ANY($1) AND state = 'fired' AND run_id = $2`, ids, runID)
}

// ReleaseExpired returns database's claims whose lease ended before now
// to fired, keeping the run each one attached.
func (s *Store) ReleaseExpired(ctx context.Context, database string, now time.Time) (int64,
	error) {
	if err := s.usable(); err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sage.review_trigger SET state = 'fired',
		claimed_by = NULL, lease_until = NULL
		WHERE database_name = $1 AND state = 'claimed' AND lease_until < $2`, database, now)
	if err != nil {
		return 0, fmt.Errorf("release expired sage.review_trigger claims of %s: %w",
			database, err)
	}
	return tag.RowsAffected(), nil
}
