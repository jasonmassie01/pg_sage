package auditchain

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Verification modes (Report.Mode).
const (
	ModeFull        = "full"        // every link from the first, and every row
	ModeIncremental = "incremental" // the links after a checkpoint, up to the head
	ModeWindow      = "window"      // an explicit range of positions
)

// CheckpointsKept bounds sage.audit_chain_checkpoint: per chain, the newest
// CheckpointsKept checkpoints are kept, plus the newest full one.
const CheckpointsKept = 100

// CheckpointDDL creates the checkpoint table (audit-03): the head a clean
// verification reached. An incremental verification starts after the
// newest one and first checks that its link still carries its hash.
const CheckpointDDL = `
CREATE TABLE IF NOT EXISTS sage.audit_chain_checkpoint (
    chain       text        NOT NULL,
    seq         bigint      NOT NULL CHECK (seq >= 0),
    hash        text        NOT NULL,
    mode        text        NOT NULL CHECK (mode IN ('full', 'incremental')),
    verified_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chain, seq)
);
`

// Checkpoint is a head a clean verification reached.
type Checkpoint struct {
	Chain      string    `json:"chain"`
	Seq        int64     `json:"seq"`
	Hash       string    `json:"hash"`
	Mode       string    `json:"mode"`
	VerifiedAt time.Time `json:"verified_at"`
}

// DB is what a checkpointed verification reads through: it opens one
// read-only snapshot for an incremental pass (a pool or a connection).
type DB interface {
	Querier
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Execer writes checkpoints.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const latestCheckpointSQL = `/* pg_sage audit_chain v1 */ SELECT seq, hash, mode,
	verified_at FROM sage.audit_chain_checkpoint WHERE chain = $1
	ORDER BY verified_at DESC, seq DESC LIMIT 1`

// LatestCheckpoint returns chain's newest checkpoint; found is false when
// it has none (or the table does not exist yet).
func LatestCheckpoint(ctx context.Context, q Querier, chain string) (Checkpoint, bool,
	error) {
	if q == nil {
		return Checkpoint{}, false, fmt.Errorf("auditchain: %s checkpoint: no database",
			chain)
	}
	cp := Checkpoint{Chain: chain}
	err := q.QueryRow(ctx, latestCheckpointSQL, chain).Scan(&cp.Seq, &cp.Hash, &cp.Mode,
		&cp.VerifiedAt)
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, &pgErr) && pgErr.Code == "42P01":
		return Checkpoint{}, false, nil
	case err != nil:
		return Checkpoint{}, false, fmt.Errorf("auditchain: %s checkpoint: %w", chain, err)
	}
	return cp, true, nil
}

// FullVerifyDue reports whether chain needs a full verification: hours is
// 0 (always full), it has no full checkpoint (or no checkpoint table yet),
// or its newest one is at least hours old by the database's clock.
func FullVerifyDue(ctx context.Context, q Querier, chain string, hours int) (bool, error) {
	if hours < 0 {
		return false, fmt.Errorf("auditchain: %s: full verification interval %d h is "+
			"negative", chain, hours)
	}
	if hours == 0 {
		return true, nil
	}
	if q == nil {
		return false, fmt.Errorf("auditchain: %s checkpoint: no database", chain)
	}
	var due bool
	err := q.QueryRow(ctx, `/* pg_sage audit_chain v1 */ SELECT coalesce(max(verified_at)
		<= now() - make_interval(hours => $2::int), true)
		FROM sage.audit_chain_checkpoint WHERE chain = $1 AND mode = 'full'`,
		chain, hours).Scan(&due)
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return true, nil // not migrated yet: verify in full, as before checkpoints
	}
	if err != nil {
		return false, fmt.Errorf("auditchain: %s full verification schedule: %w", chain, err)
	}
	return due, nil
}

// An incremental checkpoint never overwrites a full one at the same
// position: the full row dates the last full verification.
const storeCheckpointSQL = `/* pg_sage audit_chain v1 */
INSERT INTO sage.audit_chain_checkpoint (chain, seq, hash, mode) VALUES ($1, $2, $3, $4)
ON CONFLICT (chain, seq) DO UPDATE SET hash = EXCLUDED.hash, mode = EXCLUDED.mode,
    verified_at = EXCLUDED.verified_at
WHERE EXCLUDED.mode = 'full' OR sage.audit_chain_checkpoint.mode = 'incremental'`

const pruneCheckpointsSQL = `/* pg_sage audit_chain v1 */
DELETE FROM sage.audit_chain_checkpoint c WHERE c.chain = $1
AND c.seq NOT IN (SELECT seq FROM sage.audit_chain_checkpoint WHERE chain = $1
    ORDER BY verified_at DESC, seq DESC LIMIT $2)
AND c.seq NOT IN (SELECT seq FROM sage.audit_chain_checkpoint WHERE chain = $1
    AND mode = 'full' ORDER BY verified_at DESC, seq DESC LIMIT 1)`

// StoreCheckpoint records the head a clean full or incremental report
// reached, then prunes the chain's checkpoints to CheckpointsKept plus its
// newest full one. Any other report is refused: a checkpoint vouches for
// every link up to it.
func StoreCheckpoint(ctx context.Context, db Execer, rep Report) error {
	if err := checkpointable(rep); err != nil {
		return err
	}
	if db == nil {
		return fmt.Errorf("auditchain: %s checkpoint: no database", rep.Chain)
	}
	hash := rep.HeadHash
	if rep.HeadSeq == 0 {
		hash = Genesis
	}
	if _, err := db.Exec(ctx, storeCheckpointSQL, rep.Chain, rep.HeadSeq, hash,
		rep.Mode); err != nil {
		return fmt.Errorf("auditchain: %s store checkpoint %d: %w", rep.Chain, rep.HeadSeq,
			err)
	}
	if _, err := db.Exec(ctx, pruneCheckpointsSQL, rep.Chain, CheckpointsKept); err != nil {
		return fmt.Errorf("auditchain: %s prune checkpoints: %w", rep.Chain, err)
	}
	return nil
}

func checkpointable(rep Report) error {
	switch {
	case rep.Chain == "":
		return errors.New("auditchain: checkpoint: the report names no chain")
	case !rep.OK():
		return fmt.Errorf("auditchain: %s: refusing to checkpoint a report with problems",
			rep.Chain)
	case rep.Mode != ModeFull && rep.Mode != ModeIncremental:
		return fmt.Errorf("auditchain: %s: refusing to checkpoint a %q verification",
			rep.Chain, rep.Mode)
	case rep.ToSeq != rep.HeadSeq:
		return fmt.Errorf("auditchain: %s: refusing to checkpoint a verification that "+
			"stopped at %d, before the head %d", rep.Chain, rep.ToSeq, rep.HeadSeq)
	}
	return nil
}

// VerifyFromCheckpoint verifies s from its newest checkpoint up to the
// head, in one read-only snapshot. With full set, or without a checkpoint,
// it verifies the whole chain instead (streaming, with no snapshot held).
// Either way the checkpointed link must still carry its hash.
func VerifyFromCheckpoint(ctx context.Context, db DB, s Spec, full bool) (Report, error) {
	return VerifyFromCheckpointKeyed(ctx, db, s, full, nil, false)
}

// VerifyFromCheckpointKeyed is VerifyFromCheckpoint that also checks the
// chain's signed checkpoint links under key (nil checks none, and fails a
// pruned chain's full pass: nothing then vouches for its floor). pruning
// is audit.prune_below_signed_checkpoint (Window.Pruning).
func VerifyFromCheckpointKeyed(ctx context.Context, db DB, s Spec, full bool,
	key *TrustedKey, pruning bool) (Report, error) {
	if db == nil {
		return Report{}, fmt.Errorf("auditchain: %s: no database", s.Chain)
	}
	cp, found, err := LatestCheckpoint(ctx, db, s.Chain)
	if err != nil {
		return Report{}, fmt.Errorf("auditchain: verify %s: %w", s.Chain, err)
	}
	w := Window{Key: key, Pruning: pruning}
	if found {
		w.Anchor = &cp
	}
	if full || !found {
		return Verify(ctx, db, s, w)
	}
	w.FromSeq = cp.Seq + 1
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, fmt.Errorf("auditchain: verify %s: open snapshot: %w", s.Chain, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only: nothing to undo
	return Verify(ctx, tx, s, w)
}

// anchor is the checkpointed link as the chain holds it now.
type anchor struct {
	seq   int64
	hash  string
	found bool
}

// checkAnchor compares the checkpointed link with the chain: a head below
// it, a missing link or another hash is a checkpoint_mismatch.
func (v *verifier) checkAnchor(ctx context.Context, cp *Checkpoint) error {
	if cp == nil || cp.Seq <= 0 {
		return nil
	}
	v.rep.CheckpointSeq = cp.Seq
	if v.rep.HeadSeq < cp.Seq {
		v.rep.add(Problem{Kind: KindCheckpointMismatch, Seq: cp.Seq,
			Detail: fmt.Sprintf("the chain's head %d is below checkpoint %d: links "+
				"verified before were removed", v.rep.HeadSeq, cp.Seq)})
		return nil
	}
	a := &anchor{seq: cp.Seq, hash: v.rep.HeadHash, found: true}
	if v.rep.HeadSeq > cp.Seq {
		hash, found, err := LinkHashAt(ctx, v.q, v.s.Chain, cp.Seq)
		if err != nil {
			return err
		}
		a.hash, a.found = hash, found
	}
	v.anchor = a
	switch {
	case !a.found:
		v.rep.add(Problem{Kind: KindCheckpointMismatch, Seq: cp.Seq,
			Detail: fmt.Sprintf("link %d, verified at the checkpoint, is missing", cp.Seq)})
	case a.hash != cp.Hash:
		v.rep.add(Problem{Kind: KindCheckpointMismatch, Seq: cp.Seq,
			Detail: fmt.Sprintf("link %d no longer carries the hash the checkpoint "+
				"recorded: it was rewritten after it was verified", cp.Seq)})
	}
	return nil
}
