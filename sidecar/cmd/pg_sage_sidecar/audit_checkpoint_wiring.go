package main

import (
	"crypto/ed25519"
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/evidence"
)

// auditSigner signs the audit chains' checkpoints (audit-03b) with the
// evidence-signing key; nil while evidence packs are unsigned.
var auditSigner atomic.Pointer[auditchain.CheckpointSigner]

// checkpointSigner is the evidence-signing key as a checkpoint signer,
// under the key id the evidence key route serves; nil without a key.
func checkpointSigner(k evidenceKey) *auditchain.CheckpointSigner {
	if len(k.key) != ed25519.PrivateKeySize {
		return nil
	}
	pub, _ := k.key.Public().(ed25519.PublicKey)
	return &auditchain.CheckpointSigner{ID: evidence.KeyID(pub), Key: k.key}
}

// pruneBelowSignedCheckpoint is audit.prune_below_signed_checkpoint; off
// without a config.
func pruneBelowSignedCheckpoint() bool {
	return cfg != nil && cfg.Audit.PruneBelowSignedCheckpoint
}

// noteCheckpointSigning logs what the chains' checkpoints will be: signed
// or not, and whether the prune switch can take effect.
func noteCheckpointSigning(s *auditchain.CheckpointSigner) {
	switch {
	case s == nil && pruneBelowSignedCheckpoint():
		logWarn("audit", "audit.prune_below_signed_checkpoint is on, but there is no "+
			"evidence-signing key (encryption_key or audit.evidence.signing_key_env), so "+
			"checkpoints are not signed and nothing is pruned")
	case s == nil:
		logInfo("audit", "audit chain checkpoints are not signed: no evidence-signing key")
	case pruneBelowSignedCheckpoint() && (cfg == nil || len(cfg.Audit.SIEM.Sinks) == 0):
		logWarn("audit", "audit.prune_below_signed_checkpoint is on, but no SIEM sink is "+
			"configured, so nothing is pruned: links are pruned only once every sink holds "+
			"them")
	default:
		logInfo("audit", "audit chain checkpoints are signed with evidence key %s", s.ID)
	}
}
