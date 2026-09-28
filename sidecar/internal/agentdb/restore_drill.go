package agentdb

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RestoreDrillRequest is the evidence an admin records for a completed
// restore drill: what was restored, where, which checks passed, and who
// attests it. It is the only path that grants restore_verified (G8-B11).
type RestoreDrillRequest struct {
	BackupID    string
	EvidenceURI string
	Target      string
	Checks      []string
	ActorID     string
	RestoredAt  time.Time
}

// RecordRestoreDrill records a restore-drill attempt with its evidence and
// marks the backup restore_verified, citing that attempt.
func (s *Store) RecordRestoreDrill(
	ctx context.Context,
	id string,
	req RestoreDrillRequest,
) (Backup, error) {
	if strings.TrimSpace(req.BackupID) == "" || strings.TrimSpace(req.EvidenceURI) == "" ||
		strings.TrimSpace(req.Target) == "" || strings.TrimSpace(req.ActorID) == "" ||
		len(req.Checks) == 0 {
		return Backup{}, fmt.Errorf("%w: restore drill evidence is incomplete", ErrInvalid)
	}
	if _, err := s.Get(ctx, id); err != nil {
		return Backup{}, err
	}
	if req.RestoredAt.IsZero() {
		req.RestoredAt = time.Now().UTC()
	}
	evidence := map[string]any{
		"backup_id": req.BackupID, "evidence_uri": req.EvidenceURI,
		"target": req.Target, "checks": stringsAny(req.Checks),
		"actor_id": req.ActorID, "restored_at": req.RestoredAt.Format(time.RFC3339),
	}
	attempt, err := s.recordProvisionAttempt(ctx, id, provisionAttemptInput{
		Kind: "restore_drill", Status: "succeeded", Runner: "operator_attestation",
		Detail: evidence, FinishedAt: time.Now().UTC(),
	})
	if err != nil {
		return Backup{}, err
	}
	return s.writeBackup(ctx, id, BackupRequest{
		BackupID: req.BackupID, Status: "restore_verified", ArchiveURI: req.EvidenceURI,
		RestoreVerifiedAt: req.RestoredAt,
		Detail: mergeDetail(evidence, map[string]any{
			"restore_drill_attempt_id": attempt.AttemptID,
		}),
	})
}
