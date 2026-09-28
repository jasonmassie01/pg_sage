package executor

import "github.com/pg-sage/sidecar/internal/policy"

// ddlLockTimeoutMS is the lock_timeout for sql. In-transaction DDL takes the
// tighter of the policy's lock_duration_ceiling_ms and
// safety.lock_timeout_ms. CONCURRENTLY and other top-level statements keep
// the safety value: CREATE/REINDEX CONCURRENTLY waits on older transactions
// by design, and a short cap would mostly leave INVALID indexes behind.
func ddlLockTimeoutMS(sql string, safetyMS int, ceilingMS int64) int {
	if NeedsConcurrently(sql) || NeedsTopLevel(sql) {
		return safetyMS
	}
	return int(policy.EffectiveLockTimeoutMS(ceilingMS, int64(safetyMS)))
}

// dropKindForActionType types the drops the catalog can make. Index drops
// are derivable: the finding carries the index definition as its rollback,
// so they are not non_dup_object_drop and keep their earned autonomy.
func dropKindForActionType(actionType string) policy.DropKind {
	switch actionType {
	case "drop_unused_index", "revert_created_index":
		return policy.DropDerivable
	}
	return ""
}
