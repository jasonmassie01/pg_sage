// Package snapstore stores collector snapshots in sage.snapshots compactly.
//
// Catalog categories (tables, indexes, sequences, foreign keys,
// partitions, queries) are lists whose elements are mostly static
// definitions with a few moving counters. A full row (a keyframe) is
// written when a category has no usable keyframe, when the keyframe is
// keyframeMaxAge old, or when the change is too large. Every other cycle
// writes a delta row: base_id names its base row and data holds only what
// differs from it (changed fields by element identity, integer counters as
// increments, added elements, removed identities, and the order when it is
// not the base's).
//
// Bases are two levels deep at most. A checkpoint is a delta on the
// keyframe; a cycle's delta is taken on the latest checkpoint while that is
// much smaller than a delta on the keyframe (rebaseShare), and otherwise
// the cycle becomes the new checkpoint. Deltas on the keyframe alone grow
// with every counter that moved since it; chaining every cycle on the
// previous one would make a read replay hours of rows. Two levels keep
// both the bytes written and a read's work (at most two applies) small.
//
// Every reader goes through the SQL accessor sage.snapshot_data(data,
// base_id) (see DataSQL), which returns the document exactly as the
// collector wrote it, for legacy full rows and delta rows alike.
package snapstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// keyframeMaxAge bounds how long one keyframe serves as a base. It
	// also bounds how much older than the retention window a kept
	// keyframe can be.
	keyframeMaxAge = 6 * time.Hour
	// maxDeltaShare: a delta larger than 1/maxDeltaShare of its keyframe
	// is not worth it; a new keyframe is written instead.
	maxDeltaShare = 2
	// rebaseShare: a delta on the checkpoint is used while it is at most
	// 1/rebaseShare of the delta on the keyframe; otherwise the cycle
	// becomes the new checkpoint.
	rebaseShare = 4
)

// ErrInvalidRow marks a row the writer refuses (no category, no document,
// no collection time).
var ErrInvalidRow = errors.New("invalid snapshot row")

// Row is one category document of a collection cycle, as JSON.
type Row struct {
	Category string
	Data     []byte
}

// Beginner starts a transaction (pgxpool.Pool and pgx.Conn satisfy it).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// keyframe is a committed row that later rows of its category use as a
// base: a full row, or a checkpoint delta. size is its stored payload size.
// A keyframe with no list is not usable as a base.
type keyframe struct {
	id   int64
	at   time.Time
	size int
	list *catalog
}

// bases is a category's committed keyframe and latest checkpoint on it.
type bases struct {
	full       *keyframe
	checkpoint *keyframe
}

// rowPlan is how one document is stored: a delta on baseID, or a full row
// (baseID 0). next, when set, replaces the category's keyframe (a full row;
// a keyframe without a list clears it); checkpoint, when set, becomes its
// checkpoint.
type rowPlan struct {
	baseID     int64
	payload    []byte
	next       *keyframe
	checkpoint *keyframe
}

// Writer persists collection cycles. It keeps, per category, the committed
// bases; it is safe for concurrent use (calls serialize).
type Writer struct {
	mu    sync.Mutex
	bases map[string]*bases
}

// NewWriter returns a Writer with no keyframes: the first cycle of every
// catalog category is written in full.
func NewWriter() *Writer {
	return &Writer{bases: map[string]*bases{}}
}

const (
	insertFullSQL = `/* pg_sage */ INSERT INTO sage.snapshots
		(collected_at, category, data) VALUES ($1, $2, $3) RETURNING id`
	insertDeltaSQL = `/* pg_sage */ INSERT INTO sage.snapshots
		(collected_at, category, data, base_id) VALUES ($1, $2, $3, $4) RETURNING id`
)

// Persist writes the rows of one cycle collected at at, in one
// transaction. Keyframes become bases only once the transaction commits.
func (w *Writer) Persist(ctx context.Context, db Beginner, at time.Time, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	if db == nil {
		return errors.New("persist snapshot: no database to write to")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("persist snapshot: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	written := map[string]rowPlan{}
	for _, r := range rows {
		p, err := w.insert(ctx, tx, at, r)
		if err != nil {
			return err
		}
		written[r.Category] = p
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("persist snapshot: commit: %w", err)
	}
	for cat, p := range written {
		w.advance(cat, p)
	}
	return nil
}

// advance records a committed row's effect on its category's bases.
func (w *Writer) advance(category string, p rowPlan) {
	switch {
	case p.next != nil && p.next.list == nil:
		delete(w.bases, category)
	case p.next != nil:
		w.bases[category] = &bases{full: p.next}
	case p.checkpoint != nil && w.bases[category] != nil:
		w.bases[category].checkpoint = p.checkpoint
	}
}

// insert writes one row and returns its plan, with the new row's id set on
// the keyframe or checkpoint it starts.
func (w *Writer) insert(ctx context.Context, tx pgx.Tx, at time.Time, r Row) (
	rowPlan, error) {
	p, err := w.plan(r.Category, r.Data, at)
	if err != nil {
		return rowPlan{}, err
	}
	var id int64
	if p.baseID != 0 {
		err = tx.QueryRow(ctx, insertDeltaSQL, at, r.Category, p.payload, p.baseID).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, insertFullSQL, at, r.Category, p.payload).Scan(&id)
	}
	if err != nil {
		return rowPlan{}, fmt.Errorf("persist %s snapshot: %w", r.Category, err)
	}
	for _, kf := range []*keyframe{p.next, p.checkpoint} {
		if kf != nil {
			kf.id = id
		}
	}
	return p, nil
}

// plan decides how one document is stored. A document of a category
// without element identity, or one a delta cannot express, is a full row.
// A catalog document is a delta when the category has a usable keyframe
// younger than keyframeMaxAge and not newer than the document, and the
// delta on it is small enough (deltaPlan); otherwise it is a new keyframe.
func (w *Writer) plan(category string, data []byte, at time.Time) (rowPlan, error) {
	switch {
	case category == "":
		return rowPlan{}, fmt.Errorf("%w: empty category", ErrInvalidRow)
	case len(data) == 0:
		return rowPlan{}, fmt.Errorf("%w: %s: empty document", ErrInvalidRow, category)
	case at.IsZero():
		return rowPlan{}, fmt.Errorf("%w: %s: zero collection time", ErrInvalidRow, category)
	}
	list, ok, err := parseDocument(category, data)
	if !ok {
		return rowPlan{payload: data}, nil
	}
	if err != nil {
		// Stored in full; the category has no usable keyframe after it.
		return rowPlan{payload: data, next: &keyframe{}}, nil
	}
	full := rowPlan{payload: data, next: &keyframe{at: at, size: len(data), list: list}}
	b := w.bases[category]
	if b == nil || !usable(b.full, at) || at.Sub(b.full.at) >= keyframeMaxAge {
		return full, nil
	}
	if p, ok := deltaPlan(b, list, at); ok {
		return p, nil
	}
	return full, nil
}

// usable reports whether kf can serve as a base for a document at at.
func usable(kf *keyframe, at time.Time) bool {
	return kf != nil && kf.list != nil && !at.Before(kf.at)
}

// deltaPlan stores list as a delta on the latest checkpoint when that is
// at most 1/rebaseShare of the delta on the keyframe, else as a new
// checkpoint on the keyframe. It fails when the delta on the keyframe is
// more than 1/maxDeltaShare of the keyframe (a new keyframe is cheaper).
func deltaPlan(b *bases, list *catalog, at time.Time) (rowPlan, bool) {
	onFull, err := encodeDelta(b.full.list, list)
	if err != nil || len(onFull) > b.full.size/maxDeltaShare {
		return rowPlan{}, false
	}
	asCheckpoint := rowPlan{baseID: b.full.id, payload: onFull,
		checkpoint: &keyframe{at: at, size: len(onFull), list: list}}
	if !usable(b.checkpoint, at) {
		return asCheckpoint, true
	}
	onCheckpoint, err := encodeDelta(b.checkpoint.list, list)
	if err != nil || len(onCheckpoint)*rebaseShare > len(onFull) {
		return asCheckpoint, true
	}
	return rowPlan{baseID: b.checkpoint.id, payload: onCheckpoint}, true
}

// DataSQL is the SQL expression that reads a sage.snapshots row's document
// as the collector wrote it, for full and delta rows alike; NULL when a
// delta's keyframe is gone. alias qualifies the columns ("" for none).
func DataSQL(alias string) string {
	p := qualifier(alias)
	return "sage.snapshot_data(" + p + "data, " + p + "base_id)"
}

// NonEmptySQL is a SQL predicate that is true when a row's document holds
// at least one element, without reading a full row's payload: an empty or
// null full row is at most 12 bytes of jsonb; a delta row carries n.
func NonEmptySQL(alias string) string {
	p := qualifier(alias)
	return "(CASE WHEN " + p + "base_id IS NULL THEN pg_column_size(" + p +
		"data) > 12 ELSE (" + p + "data->>'n')::int > 0 END)"
}

func qualifier(alias string) string {
	if alias == "" {
		return ""
	}
	return alias + "."
}
