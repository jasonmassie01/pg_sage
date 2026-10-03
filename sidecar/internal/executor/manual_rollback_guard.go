package executor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/recommendation"
)

// ErrRollbackMismatch refuses a CREATE INDEX whose rollback would drop a
// different index than the one created.
var ErrRollbackMismatch = errors.New("rollback does not drop the index being created")

// checkIndexRollback refuses an operator CREATE INDEX whose rollback is not
// the drop of exactly that index (lifeos 1.8.3, finding 18007: a stale
// rollback naming idx_memories_active_partial). A revert of such an action
// would drop someone else's index, so the action does not run. An unnamed
// or unparseable create, or one without a rollback, is not judged here.
func checkIndexRollback(sql, rollbackSQL string) error {
	if categorizeAction(sql) != "create_index" || strings.TrimSpace(rollbackSQL) == "" {
		return nil
	}
	if _, named := recommendation.DerivedIndexInverse(sql); !named {
		return nil
	}
	if recommendation.InverseDropsCreatedIndex(sql, rollbackSQL) {
		return nil
	}
	return fmt.Errorf("%w: %q does not undo %q", ErrRollbackMismatch, rollbackSQL, sql)
}
