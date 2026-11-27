package auditjob

import (
	"context"
	"errors"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// trustedKey is the key the chains' signed checkpoints are verified with:
// the signer's, nil without one.
func (src Source) trustedKey() *auditchain.TrustedKey {
	if src.Signer == nil {
		return nil
	}
	k := src.Signer.Trusted()
	return &k
}

// signAndPrune runs after a clean report is checkpointed: it signs the
// head into the chain (exported to the SIEM as a checkpoint link) and,
// opted in and after a full pass, prunes below the newest signed
// checkpoint every sink has received (audit-03b).
func signAndPrune(ctx context.Context, src Source, spec auditchain.Spec,
	rep auditchain.Report) error {
	if src.Signer == nil {
		return nil
	}
	if _, _, err := auditchain.AppendSignedCheckpoint(ctx, src.Pool, rep,
		*src.Signer); err != nil {
		return fmt.Errorf("auditjob: %s: sign checkpoint: %w", src.Name, err)
	}
	if !src.PruneBelowSignedCheckpoint || rep.Mode != auditchain.ModeFull {
		return nil
	}
	exported, ok, err := exportedThrough(ctx, src, spec.Chain)
	if err != nil {
		return fmt.Errorf("auditjob: %s: prune %s: SIEM cursors: %w", src.Name, spec.Chain,
			err)
	}
	if !ok {
		return nil // no SIEM sink receives this chain: nothing off-box to prune against
	}
	_, err = auditchain.PruneBelowSignedCheckpoint(ctx, src.Pool, spec,
		auditchain.PruneRequest{Key: src.Signer.Trusted(), ExportedThrough: exported})
	if errors.Is(err, auditchain.ErrNoSignedCheckpoint) ||
		errors.Is(err, auditchain.ErrCheckpointNotExported) {
		return nil // the checkpoint link has not reached every sink yet
	}
	if err != nil {
		return fmt.Errorf("auditjob: %s: prune %s: %w", src.Name, spec.Chain, err)
	}
	return nil
}

// exportedThrough is the lowest position every SIEM sink of chain has
// received; ok is false when no sink receives it.
func exportedThrough(ctx context.Context, src Source, chain string) (int64, bool, error) {
	if src.Cursors == nil {
		return 0, false, nil
	}
	cursors, err := src.Cursors.Cursors(ctx, src.Name, chain)
	if err != nil || len(cursors) == 0 {
		return 0, false, err
	}
	lowest := cursors[0].Seq
	for _, c := range cursors[1:] {
		lowest = min(lowest, c.Seq)
	}
	return lowest, true, nil
}
