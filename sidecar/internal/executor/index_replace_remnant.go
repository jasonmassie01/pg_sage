package executor

import (
	"context"
	"fmt"
)

// The replacement's remnants go through the one remnant handler
// (executor-05): its build records the OID it registers, so even a resume
// after a crash drops only that object.

// recordReplaceBuildOID persists the OID the replacement's build
// registered while the row is still creating.
func (e *Executor) recordReplaceBuildOID(ctx context.Context, id, oid int64) {
	if _, err := e.pool.Exec(ctx, `/* pg_sage executor */ UPDATE sage.index_replace
		SET new_index_oid = $2, updated_at = now()
		WHERE id = $1 AND state = 'creating'`, id, oid); err != nil {
		e.logFn("executor", "record the build OID %d of replacement %d: %v", oid, id, err)
	}
}

// dropBoundRemnant drops index only while it is still OID oid on table
// and INVALID; a VALID index, or one re-created under that name, is kept.
func (e *Executor) dropBoundRemnant(
	ctx context.Context, index string, oid int64, table string,
) (*remnantCleanup, error) {
	tableOID, err := e.indexOID(ctx, table)
	if err != nil {
		return nil, err
	}
	drop := "DROP INDEX CONCURRENTLY IF EXISTS " + index
	run := e.remnantRunFor(drop, WithLockTimeout(e.lockTimeoutMS(drop, ActionPolicyDecision{})))
	cleanup := run.dropRemnants(ctx, []remnantCandidate{
		{Index: index, OID: oid, TableOID: tableOID}})
	e.logFn("executor", "remnant cleanup of %s: %s", index, cleanup.summary())
	if cleanup.Outcome == remnantCleanupFailed {
		return cleanup, fmt.Errorf("%s: %s", remnantCleanupFailed, cleanup.summary())
	}
	return cleanup, nil
}

// resumeDropRemnant settles a build interrupted by a crash: its INVALID
// remnant is dropped when the build's OID was recorded, and otherwise left
// to the invalid_index finding.
func (e *Executor) resumeDropRemnant(ctx context.Context, rec *indexReplaceRecord) error {
	reason := "interrupted during the build; its INVALID remnant was dropped"
	if rec.NewOID <= 0 {
		reason = "interrupted during the build before its OID was recorded; the INVALID " +
			rec.Plan.NewIndex + " is left to the invalid_index finding"
		e.logFn("executor", "replacement %d: %s", rec.ID, reason)
	} else {
		cleanup, err := e.dropBoundRemnant(ctx, rec.Plan.NewIndex, rec.NewOID, rec.Plan.Table)
		if err != nil {
			return err
		}
		if cleanup.Outcome != remnantDropped {
			reason = "interrupted during the build; " + cleanup.summary()
		}
	}
	return e.moveReplace(ctx, rec, replaceCreateFailed, reason, replaceCreating)
}
