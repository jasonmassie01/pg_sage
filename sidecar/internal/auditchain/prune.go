package auditchain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Prune refusals.
var (
	// ErrNoSignedCheckpoint: the chain holds no signed checkpoint link.
	ErrNoSignedCheckpoint = errors.New("auditchain: no signed checkpoint to prune below")
	// ErrCheckpointNotExported: no signed checkpoint link has reached every
	// SIEM sink yet, so the links below it have no off-box copy.
	ErrCheckpointNotExported = errors.New("auditchain: no signed checkpoint is exported " +
		"to every SIEM sink yet")
	// ErrPruneUnsafe: the links below the floor hold evidence of a problem.
	ErrPruneUnsafe = errors.New("auditchain: pruning would erase evidence")
)

// PruneMark is a prune link's state: links below Floor were removed on the
// strength of the signed checkpoint of link Checkpoint, carried by the
// checkpoint link at CheckpointLink.
type PruneMark struct {
	Floor          int64 `json:"floor"`
	Checkpoint     int64 `json:"checkpoint"`
	CheckpointLink int64 `json:"checkpoint_link"`
	Pruned         int64 `json:"pruned"`
}

// ParsePruneMark reads a prune link's state.
func ParsePruneMark(payload string) (PruneMark, error) {
	var m PruneMark
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return PruneMark{}, fmt.Errorf("auditchain: prune payload: %w", err)
	}
	if m.Floor < 2 || m.Checkpoint < m.Floor || m.CheckpointLink <= m.Checkpoint ||
		m.Pruned < 1 {
		return PruneMark{}, fmt.Errorf("auditchain: prune payload %+v is inconsistent", m)
	}
	return m, nil
}

// PruneRequest is what a prune may rely on: the key the checkpoint must be
// signed with, and the lowest position every SIEM sink has received.
type PruneRequest struct {
	Key             TrustedKey
	ExportedThrough int64
}

// PruneResult says what a prune did; Pruned 0 means nothing was removed
// (and no prune link written).
type PruneResult struct {
	Floor          int64 `json:"floor"`
	Checkpoint     int64 `json:"checkpoint"`
	CheckpointLink int64 `json:"checkpoint_link"`
	Pruned         int64 `json:"pruned"`
	PruneLink      int64 `json:"prune_link,omitempty"`
}

const exportedCheckpointSQL = `/* pg_sage audit_chain v1 */ SELECT seq, state
	FROM sage.audit_chain_link WHERE chain = $1 AND op = 'C' AND seq <= $2
	ORDER BY seq DESC LIMIT 1`

const anyCheckpointSQL = `/* pg_sage audit_chain v1 */ SELECT EXISTS (SELECT 1
	FROM sage.audit_chain_link WHERE chain = $1 AND op = 'C')`

const lowestLinkSQL = `/* pg_sage audit_chain v1 */ SELECT seq FROM sage.audit_chain_link
	WHERE chain = $1 ORDER BY seq LIMIT 1`

const deleteBelowSQL = `/* pg_sage audit_chain v1 */ DELETE FROM sage.audit_chain_link
	WHERE chain = $1 AND seq < $2`

// vanishedRowSQL finds a row inserted below the floor that is gone with no
// delete link and no later truncate: the verifier's row_missing.
const vanishedRowSQL = `/* pg_sage audit_chain v1 */ SELECT i.row_id, i.seq
	FROM sage.audit_chain_link i WHERE i.chain = $1 AND i.seq < $2 AND i.op = 'I'
	AND NOT EXISTS (SELECT 1 FROM sage.audit_chain_link d WHERE d.chain = $1
		AND d.row_id = i.row_id AND d.op = 'D')
	AND NOT EXISTS (SELECT 1 FROM sage.audit_chain_link t WHERE t.chain = $1
		AND t.op = 'T' AND t.seq > i.seq)
	ORDER BY i.seq LIMIT 1`

// PruneBelowSignedCheckpoint removes the links of s below its newest
// signed checkpoint that every SIEM sink has received (opt-in,
// audit.prune_below_signed_checkpoint), and records the removal as a
// prune link. Verification then starts at the floor, anchored by that
// checkpoint. The floor never passes the first link of a row still in
// the table, so every live row keeps all its links; and a row inserted
// below the floor that vanished without a delete link refuses the prune
// (ErrPruneUnsafe). Prunes of one chain are serialised; writers are not
// held while the links are deleted.
func PruneBelowSignedCheckpoint(ctx context.Context, db DB, s Spec, req PruneRequest) (
	PruneResult, error) {
	if err := s.validate(); err != nil {
		return PruneResult{}, fmt.Errorf("auditchain: invalid spec: %w", err)
	}
	if db == nil {
		return PruneResult{}, fmt.Errorf("auditchain: %s prune: no database", s.Chain)
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PruneResult{}, fmt.Errorf("auditchain: %s prune: begin: %w", s.Chain, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return PruneResult{}, fmt.Errorf("auditchain: %s prune: lock_timeout: %w", s.Chain,
			err)
	}
	if _, err := tx.Exec(ctx, `/* pg_sage audit_chain v1 */ SELECT pg_advisory_xact_lock(
		$1, hashtext('audit_chain prune:' || $2))`, lockClass, s.Chain); err != nil {
		return PruneResult{}, fmt.Errorf("auditchain: %s prune: lock: %w", s.Chain, err)
	}
	res, err := pruneTx(ctx, tx, s, req)
	if err != nil || res.Pruned == 0 {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PruneResult{}, fmt.Errorf("auditchain: %s prune: commit: %w", s.Chain, err)
	}
	return res, nil
}

func pruneTx(ctx context.Context, tx pgx.Tx, s Spec, req PruneRequest) (PruneResult,
	error) {
	cp, cpLink, err := exportedCheckpoint(ctx, tx, s.Chain, req)
	if err != nil {
		return PruneResult{}, err
	}
	res := PruneResult{Checkpoint: cp.Seq, CheckpointLink: cpLink}
	var lowest int64
	if err := tx.QueryRow(ctx, lowestLinkSQL, s.Chain).Scan(&lowest); err != nil {
		return res, fmt.Errorf("auditchain: %s prune: lowest link: %w", s.Chain, err)
	}
	if cp.Seq <= lowest {
		res.Floor = lowest
		return res, nil // already pruned up to this checkpoint
	}
	hash, found, err := LinkHashAt(ctx, tx, s.Chain, cp.Seq)
	if err != nil {
		return res, err
	}
	if !found || hash != cp.Hash {
		return res, fmt.Errorf("%w: %s link %d no longer carries the hash its signed "+
			"checkpoint recorded; verify the chain in full", ErrPruneUnsafe, s.Chain, cp.Seq)
	}
	if res.Floor, err = pruneFloor(ctx, tx, s, cp.Seq); err != nil {
		return res, err
	}
	if res.Floor <= lowest {
		return res, nil
	}
	if err := checkVanished(ctx, tx, s.Chain, res.Floor); err != nil {
		return res, err
	}
	tag, err := tx.Exec(ctx, deleteBelowSQL, s.Chain, res.Floor)
	if err != nil {
		return res, fmt.Errorf("auditchain: %s prune below %d: %w", s.Chain, res.Floor, err)
	}
	res.Pruned = tag.RowsAffected()
	res.PruneLink, err = appendMarker(ctx, tx, s.Chain, OpPrune, PruneMark{Floor: res.Floor,
		Checkpoint: cp.Seq, CheckpointLink: cpLink, Pruned: res.Pruned})
	if err != nil {
		return res, err
	}
	return res, nil
}

// exportedCheckpoint is the newest checkpoint link every sink received,
// verified under req.Key.
func exportedCheckpoint(ctx context.Context, tx pgx.Tx, chain string, req PruneRequest) (
	SignedCheckpoint, int64, error) {
	var seq int64
	var state string
	err := tx.QueryRow(ctx, exportedCheckpointSQL, chain, req.ExportedThrough).Scan(&seq,
		&state)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, anyCheckpointSQL, chain).Scan(&exists); err != nil {
			return SignedCheckpoint{}, 0, fmt.Errorf("auditchain: %s prune: %w", chain, err)
		}
		if exists {
			return SignedCheckpoint{}, 0, fmt.Errorf("%w: %s is exported through %d",
				ErrCheckpointNotExported, chain, req.ExportedThrough)
		}
		return SignedCheckpoint{}, 0, fmt.Errorf("%w: %s", ErrNoSignedCheckpoint, chain)
	}
	if err != nil {
		return SignedCheckpoint{}, 0, fmt.Errorf("auditchain: %s prune: checkpoint: %w",
			chain, err)
	}
	cp, err := ParseSignedCheckpoint(state)
	if err != nil {
		return SignedCheckpoint{}, 0, fmt.Errorf("%w: %s checkpoint link %d: %v",
			ErrCheckpointSignature, chain, seq, err)
	}
	if cp.Chain != chain || cp.Seq >= seq {
		return SignedCheckpoint{}, 0, fmt.Errorf("%w: %s checkpoint link %d signs %s link "+
			"%d", ErrCheckpointSignature, chain, seq, cp.Chain, cp.Seq)
	}
	if err := cp.Verify(req.Key); err != nil {
		return SignedCheckpoint{}, 0, fmt.Errorf("auditchain: %s prune: %w", chain, err)
	}
	return cp, seq, nil
}

// pruneFloor is the lowest link a prune keeps: the signed checkpoint's
// link, or the first link of a row still in the table when that is lower.
func pruneFloor(ctx context.Context, tx pgx.Tx, s Spec, checkpoint int64) (int64, error) {
	sql := fmt.Sprintf(`/* pg_sage audit_chain v1 */ SELECT min(fl.seq) FROM %s t
		CROSS JOIN LATERAL (SELECT l.seq FROM sage.audit_chain_link l
			WHERE l.chain = $1 AND l.row_id = t.id ORDER BY l.seq LIMIT 1) fl`, s.Table)
	var first *int64
	if err := tx.QueryRow(ctx, sql, s.Chain).Scan(&first); err != nil {
		return 0, fmt.Errorf("auditchain: %s prune: first link of a live row: %w", s.Chain,
			err)
	}
	if first != nil && *first < checkpoint {
		return *first, nil
	}
	return checkpoint, nil
}

func checkVanished(ctx context.Context, tx pgx.Tx, chain string, floor int64) error {
	var row, seq int64
	err := tx.QueryRow(ctx, vanishedRowSQL, chain, floor).Scan(&row, &seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auditchain: %s prune: vanished rows: %w", chain, err)
	}
	return fmt.Errorf("%w: %s row %d (link %d) is gone and no delete or truncate link "+
		"records it; verify the chain in full and investigate before pruning",
		ErrPruneUnsafe, chain, row, seq)
}
