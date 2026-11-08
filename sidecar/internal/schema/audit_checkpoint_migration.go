package schema

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// auditCheckpointLockTimeoutSQL bounds the wait for a lock: creating the
// table takes none another session holds, so a wait means a stuck DDL.
const auditCheckpointLockTimeoutSQL = "SET LOCAL lock_timeout = '5s'"

// migrateAuditCheckpoint creates sage.audit_chain_checkpoint (audit-03):
// the heads clean verifications reached, which incremental verification
// starts after. Idempotent, under lock_timeout.
func migrateAuditCheckpoint(ctx context.Context, db bootstrapDB) error {
	qctx, cancel := context.WithTimeout(ctx, migrationBatchTimeout)
	defer cancel()
	tx, err := db.Begin(qctx)
	if err != nil {
		return fmt.Errorf("audit checkpoint migration: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(qctx) }() // no-op after commit
	if _, err := tx.Exec(qctx, auditCheckpointLockTimeoutSQL); err != nil {
		return fmt.Errorf("audit checkpoint migration: set lock_timeout: %w", err)
	}
	if _, err := tx.Exec(qctx, auditchain.CheckpointDDL); err != nil {
		return fmt.Errorf("audit checkpoint migration: %w", err)
	}
	if err := tx.Commit(qctx); err != nil {
		return fmt.Errorf("audit checkpoint migration: commit: %w", err)
	}
	return nil
}
