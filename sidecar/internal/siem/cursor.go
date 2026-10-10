package siem

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrFenced refuses export by a sidecar whose leader lease is gone: a
// successor may already be exporting.
var ErrFenced = errors.New("siem: leader lease lost; export stopped")

// Fence is the leader lease export runs under; the zero Fence is unfenced
// (leader election disabled).
type Fence struct {
	Scope  string
	Holder string
	Epoch  int64
}

// CursorStore keeps each sink's position per source and chain on the
// control database, so a restarted or new leader resumes where export
// stopped.
type CursorStore struct {
	pool *pgxpool.Pool
}

// NewCursorStore returns the cursor store on pool.
func NewCursorStore(pool *pgxpool.Pool) *CursorStore { return &CursorStore{pool: pool} }

var errNoControl = errors.New("siem: no control database for export cursors")

const fenceSQL = `/* pg_sage siem v1 */ SELECT 1 FROM sage.fleet_leader_lease
	WHERE scope = $1 AND holder = $2 AND epoch = $3 AND expires_at > now() FOR SHARE`

// Load returns the last exported position (0 when none).
func (c *CursorStore) Load(ctx context.Context, sink, source, chain string) (int64, error) {
	if c == nil || c.pool == nil {
		return 0, errNoControl
	}
	var seq int64
	err := c.pool.QueryRow(ctx, `/* pg_sage siem v1 */ SELECT seq FROM sage.siem_cursor
		WHERE sink = $1 AND source = $2 AND chain = $3`, sink, source, chain).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("siem: load cursor %s/%s/%s: %w", sink, source, chain, err)
	}
	return seq, nil
}

// CheckFence reports ErrFenced when f no longer holds the lease.
func (c *CursorStore) CheckFence(ctx context.Context, f Fence) error {
	if f.Holder == "" {
		return nil
	}
	if c == nil || c.pool == nil {
		return errNoControl
	}
	var one int
	err := c.pool.QueryRow(ctx, fenceSQL, f.Scope, f.Holder, f.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return fmt.Errorf("siem: check lease: %w", err)
	}
	return nil
}

// Save moves a cursor forward (never back) under the fence.
func (c *CursorStore) Save(ctx context.Context, f Fence, sink, source, chain string,
	seq int64) error {
	if c == nil || c.pool == nil {
		return errNoControl
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("siem: save cursor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if f.Holder != "" {
		var one int
		err := tx.QueryRow(ctx, fenceSQL, f.Scope, f.Holder, f.Epoch).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return fmt.Errorf("siem: save cursor: check lease: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `/* pg_sage siem v1 */ INSERT INTO sage.siem_cursor
		(sink, source, chain, seq) VALUES ($1, $2, $3, $4)
		ON CONFLICT (sink, source, chain) DO UPDATE SET seq = EXCLUDED.seq,
		updated_at = now() WHERE sage.siem_cursor.seq < EXCLUDED.seq`,
		sink, source, chain, seq); err != nil {
		return fmt.Errorf("siem: save cursor %s/%s/%s: %w", sink, source, chain, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("siem: save cursor: commit: %w", err)
	}
	return nil
}
