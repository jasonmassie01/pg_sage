package upgradecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// storeTag marks the store's statements as pg_sage's own.
const storeTag = "/* pg_sage upgrade_brief */ "

// storeTimeout bounds each statement of a store.
const storeTimeout = 5 * time.Second

// ErrInvalidBrief is a brief the store refuses (no database, target or
// hash).
var ErrInvalidBrief = errors.New("upgradecheck: invalid upgrade brief")

// StoredBrief is the store's answer: the latest row of the brief's
// database and target, whether this call added it, and when its content
// was first stored (the brief's age).
type StoredBrief struct {
	ID      int64     `json:"id"`
	Stored  bool      `json:"stored"`
	BuiltAt time.Time `json:"built_at"`
	Hash    string    `json:"hash"`
}

// StoreBrief keeps b in sage.upgrade_briefs only when its hash differs
// from the latest stored brief of its database and target major
// (delta-only), so older briefs stay. Concurrent stores of one database
// and target serialize on a transaction advisory lock.
func StoreBrief(ctx context.Context, pool *pgxpool.Pool, b Brief) (StoredBrief, error) {
	if pool == nil {
		return StoredBrief{}, ErrNoPool
	}
	if b.Database == "" || b.Target.Major <= 0 || len(b.Hash) != 64 {
		return StoredBrief{}, fmt.Errorf("%w: database %q, target %d, hash %q",
			ErrInvalidBrief, b.Database, b.Target.Major, b.Hash)
	}
	doc, err := json.Marshal(b)
	if err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief: encode: %w", err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // no-op after commit
	got, err := storeInTx(ctx, tx, b, doc)
	if err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief of %s: %w", b.Database, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief of %s: commit: %w",
			b.Database, err)
	}
	return got, nil
}

func storeInTx(ctx context.Context, tx pgx.Tx, b Brief, doc []byte) (StoredBrief, error) {
	ms := strconv.FormatInt(storeTimeout.Milliseconds(), 10)
	if _, err := tx.Exec(ctx, storeTag+"SET LOCAL statement_timeout = "+ms); err != nil {
		return StoredBrief{}, err
	}
	if _, err := tx.Exec(ctx, storeTag+"SET LOCAL lock_timeout = "+ms); err != nil {
		return StoredBrief{}, err
	}
	key := "pg_sage upgrade_brief " + b.Database + " " + strconv.Itoa(b.Target.Major)
	if _, err := tx.Exec(ctx, storeTag+`SELECT pg_catalog.pg_advisory_xact_lock(
		pg_catalog.hashtextextended($1, 0))`, key); err != nil {
		return StoredBrief{}, err
	}
	out := StoredBrief{Hash: b.Hash}
	var latest string
	err := tx.QueryRow(ctx, storeTag+`SELECT id, hash, built_at FROM sage.upgrade_briefs
		WHERE database = $1 AND target_major = $2
		ORDER BY built_at DESC, id DESC LIMIT 1`, b.Database, b.Target.Major).
		Scan(&out.ID, &latest, &out.BuiltAt)
	switch {
	case err == nil && latest == b.Hash:
		return out, nil
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return StoredBrief{}, err
	}
	out.Stored = true
	err = tx.QueryRow(ctx, storeTag+`INSERT INTO sage.upgrade_briefs
		(database, target_major, brief, hash) VALUES ($1, $2, $3, $4)
		RETURNING id, built_at`, b.Database, b.Target.Major, doc, b.Hash).
		Scan(&out.ID, &out.BuiltAt)
	return out, err
}
