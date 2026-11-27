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

// ErrNoCursor means a sink has exported nothing of that source's chain.
var ErrNoCursor = errors.New("siem: no export cursor for that sink, source and chain")

// ErrInvalidReanchor refuses a re-anchor that would skip or invent links.
var ErrInvalidReanchor = errors.New("siem: invalid re-anchor")

// Fence is the leader lease export runs under; the zero Fence is unfenced
// (leader election disabled).
type Fence struct {
	Scope  string
	Holder string
	Epoch  int64
}

// Cursor is one sink's export position on one chain of one source: the
// last exported link's position and hash.
type Cursor struct {
	Sink   string `json:"sink"`
	Source string `json:"source"`
	Chain  string `json:"chain"`
	Seq    int64  `json:"seq"`
	// Hash is "" for a cursor saved before hashes were kept (or at 0).
	Hash string `json:"hash,omitempty"`
	// ForkTo is the furthest position a re-anchor sent back over: links
	// up to it are re-exported labelled "fork".
	ForkTo int64 `json:"fork_to,omitempty"`
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

// LoadCursor returns a sink's cursor (at 0 when it exported nothing yet).
func (c *CursorStore) LoadCursor(ctx context.Context, sink, source, chain string) (
	Cursor, error) {
	cur := Cursor{Sink: sink, Source: source, Chain: chain}
	if c == nil || c.pool == nil {
		return cur, errNoControl
	}
	var hash *string
	var forkTo *int64
	err := c.pool.QueryRow(ctx, `/* pg_sage siem v1 */ SELECT seq, hash, fork_to
		FROM sage.siem_cursor WHERE sink = $1 AND source = $2 AND chain = $3`,
		sink, source, chain).Scan(&cur.Seq, &hash, &forkTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return cur, nil
	}
	if err != nil {
		return cur, fmt.Errorf("siem: load cursor %s/%s/%s: %w", sink, source, chain, err)
	}
	if hash != nil {
		cur.Hash = *hash
	}
	if forkTo != nil {
		cur.ForkTo = *forkTo
	}
	return cur, nil
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

// fenced runs fn in one transaction that first checks f still holds the
// lease, so no write lands after a successor took over.
func (c *CursorStore) fenced(ctx context.Context, f Fence, what string,
	fn func(pgx.Tx) error) error {
	if c == nil || c.pool == nil {
		return errNoControl
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("siem: %s: begin: %w", what, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if f.Holder != "" {
		var one int
		err := tx.QueryRow(ctx, fenceSQL, f.Scope, f.Holder, f.Epoch).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		if err != nil {
			return fmt.Errorf("siem: %s: check lease: %w", what, err)
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("siem: %s: commit: %w", what, err)
	}
	return nil
}

// Save moves a cursor forward (never back) to the link at seq with hash,
// under the fence.
func (c *CursorStore) Save(ctx context.Context, f Fence, sink, source, chain string,
	seq int64, hash string) error {
	return c.fenced(ctx, f, "save cursor", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage siem v1 */ INSERT INTO sage.siem_cursor
			(sink, source, chain, seq, hash) VALUES ($1, $2, $3, $4, nullif($5, ''))
			ON CONFLICT (sink, source, chain) DO UPDATE SET seq = EXCLUDED.seq,
			hash = EXCLUDED.hash, updated_at = now()
			WHERE sage.siem_cursor.seq < EXCLUDED.seq`,
			sink, source, chain, seq, hash); err != nil {
			return fmt.Errorf("siem: save cursor %s/%s/%s: %w", sink, source, chain, err)
		}
		return nil
	})
}

// BackfillHash records the hash of a cursor saved before hashes were
// kept, once its link was found at the cursor's position.
func (c *CursorStore) BackfillHash(ctx context.Context, f Fence, sink, source,
	chain string, seq int64, hash string) error {
	return c.fenced(ctx, f, "backfill cursor hash", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage siem v1 */ UPDATE sage.siem_cursor
			SET hash = $5 WHERE sink = $1 AND source = $2 AND chain = $3 AND seq = $4
			AND hash IS NULL`, sink, source, chain, seq, hash); err != nil {
			return fmt.Errorf("siem: backfill cursor %s/%s/%s: %w", sink, source, chain,
				err)
		}
		return nil
	})
}

// Reanchor moves a cursor back to the link at seq with hash (0 and "" for
// the chain's start), under the fence. It is the only way a cursor moves
// back, never forward, and it records how far the cursor had gone, so
// that the links re-exported up to there carry the fork label. It returns
// the cursor as it was.
func (c *CursorStore) Reanchor(ctx context.Context, f Fence, sink, source, chain string,
	seq int64, hash string) (Cursor, error) {
	prev := Cursor{Sink: sink, Source: source, Chain: chain}
	err := c.fenced(ctx, f, "re-anchor cursor", func(tx pgx.Tx) error {
		var oldHash *string
		var forkTo *int64
		err := tx.QueryRow(ctx, `/* pg_sage siem v1 */ SELECT seq, hash, fork_to
			FROM sage.siem_cursor WHERE sink = $1 AND source = $2 AND chain = $3
			FOR UPDATE`, sink, source, chain).Scan(&prev.Seq, &oldHash, &forkTo)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s/%s/%s", ErrNoCursor, sink, source, chain)
		}
		if err != nil {
			return fmt.Errorf("siem: re-anchor %s/%s/%s: read: %w", sink, source, chain, err)
		}
		if oldHash != nil {
			prev.Hash = *oldHash
		}
		if forkTo != nil {
			prev.ForkTo = *forkTo
		}
		if err := reanchorAllowed(prev, seq, hash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `/* pg_sage siem v1 */ UPDATE sage.siem_cursor
			SET seq = $4, hash = nullif($5, ''), fork_to = greatest(coalesce(fork_to, 0),
			seq), updated_at = now() WHERE sink = $1 AND source = $2 AND chain = $3`,
			sink, source, chain, seq, hash); err != nil {
			return fmt.Errorf("siem: re-anchor %s/%s/%s: %w", sink, source, chain, err)
		}
		return nil
	})
	return prev, err
}

// reanchorAllowed refuses the re-anchors that provably skip links never
// exported: past the cursor, or at the cursor's own position when another
// link now holds it. Below the cursor it cannot tell which links the SIEM
// holds; choosing that position is the admin's call (1 is always safe).
func reanchorAllowed(prev Cursor, seq int64, hash string) error {
	if seq > prev.Seq {
		return fmt.Errorf("%w: position %d is past the cursor (%d): links %d-%d "+
			"were never exported", ErrInvalidReanchor, seq, prev.Seq, prev.Seq+1, seq)
	}
	if seq == prev.Seq && seq > 0 && prev.Hash != "" && hash != prev.Hash {
		return fmt.Errorf("%w: the link at %d is not the one exported there; resume "+
			"at a lower from_seq so it is exported", ErrInvalidReanchor, seq)
	}
	return nil
}
