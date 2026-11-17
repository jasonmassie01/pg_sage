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
// database and target, whether this call added it, when its content was
// first stored (the brief's age), and the rehearsal the row carries.
type StoredBrief struct {
	ID        int64            `json:"id"`
	Stored    bool             `json:"stored"`
	BuiltAt   time.Time        `json:"built_at"`
	Hash      string           `json:"hash"`
	Rehearsal *RehearsalReport `json:"rehearsal,omitempty"`
}

// StoreBrief keeps b in sage.upgrade_briefs only when its hash differs
// from the latest stored brief of its database and target major
// (delta-only), so older briefs stay. A new row carries the latest row's
// rehearsal when b has none. Concurrent stores of one database and target
// serialize on a transaction advisory lock.
func StoreBrief(ctx context.Context, pool *pgxpool.Pool, b Brief) (StoredBrief, error) {
	return storeBrief(ctx, pool, b, nil)
}

func validStoredBrief(pool *pgxpool.Pool, b Brief) error {
	if pool == nil {
		return ErrNoPool
	}
	if b.Database == "" || b.Target.Major <= 0 || len(b.Hash) != 64 {
		return fmt.Errorf("%w: database %q, target %d, hash %q",
			ErrInvalidBrief, b.Database, b.Target.Major, b.Hash)
	}
	return nil
}

// storeBrief stores b, then, with after set, runs after on the stored row
// in the same transaction.
func storeBrief(ctx context.Context, pool *pgxpool.Pool, b Brief,
	after func(pgx.Tx, *StoredBrief) error) (StoredBrief, error) {
	if err := validStoredBrief(pool, b); err != nil {
		return StoredBrief{}, err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() // no-op after commit
	got, err := storeInTx(ctx, tx, b)
	if err == nil && after != nil {
		err = after(tx, &got)
	}
	if err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief of %s: %w", b.Database, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredBrief{}, fmt.Errorf("store upgrade brief of %s: commit: %w",
			b.Database, err)
	}
	return got, nil
}

func storeInTx(ctx context.Context, tx pgx.Tx, b Brief) (StoredBrief, error) {
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
	var prev []byte
	err := tx.QueryRow(ctx, storeTag+`SELECT id, hash, built_at, brief->'rehearsal'
		FROM sage.upgrade_briefs WHERE database = $1 AND target_major = $2
		ORDER BY built_at DESC, id DESC LIMIT 1`, b.Database, b.Target.Major).
		Scan(&out.ID, &latest, &out.BuiltAt, &prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return StoredBrief{}, err
	}
	if b.Rehearsal == nil && len(prev) > 0 {
		var carried RehearsalReport
		if err := json.Unmarshal(prev, &carried); err != nil {
			return StoredBrief{}, fmt.Errorf("decode the stored rehearsal: %w", err)
		}
		b.Rehearsal = &carried
	}
	out.Rehearsal = b.Rehearsal
	if err == nil && latest == b.Hash {
		return out, refreshQueryTime(ctx, tx, out.ID, b.QueryTime)
	}
	doc, err := json.Marshal(b)
	if err != nil {
		return StoredBrief{}, fmt.Errorf("encode: %w", err)
	}
	out.Stored = true
	err = tx.QueryRow(ctx, storeTag+`INSERT INTO sage.upgrade_briefs
		(database, target_major, brief, hash) VALUES ($1, $2, $3, $4)
		RETURNING id, built_at`, b.Database, b.Target.Major, doc, b.Hash).
		Scan(&out.ID, &out.BuiltAt)
	return out, err
}

// refreshQueryTime carries a known, non-empty query-time share onto the
// stored row id (drill-02c): the share is not part of the hash, so an
// unchanged brief keeps its row and the post-upgrade ANALYZE still reads
// the latest share. An unknown or empty share (pg_stat_statements absent
// or reset) keeps the stored one.
func refreshQueryTime(ctx context.Context, tx pgx.Tx, id int64, q QueryTimeSection) error {
	if q.State != SectionKnown || len(q.Items) == 0 {
		return nil
	}
	doc, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("encode the query-time share: %w", err)
	}
	if _, err := tx.Exec(ctx, storeTag+`UPDATE sage.upgrade_briefs
		SET brief = pg_catalog.jsonb_set(brief, '{query_time_share}', $2::jsonb)
		WHERE id = $1`, id, doc); err != nil {
		return fmt.Errorf("refresh the query-time share: %w", err)
	}
	return nil
}
