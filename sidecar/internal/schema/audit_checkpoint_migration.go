package schema

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// migrateAuditCheckpoint creates sage.audit_chain_checkpoint (audit-03):
// the heads clean verifications reached, which incremental verification
// starts after. Idempotent, under lock_timeout, as a ledger step
// (config-04b).
func migrateAuditCheckpoint(ctx context.Context, db bootstrapDB) error {
	if _, err := runMigrationStep(ctx, db, migrationStep{
		name: "auditchain.CheckpointDDL",
		sql:  stepLockTimeoutSQL + ";" + auditchain.CheckpointDDL,
	}); err != nil {
		return fmt.Errorf("audit checkpoint migration: %w", err)
	}
	return nil
}
