package executor

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/notify"
)

func isDDLMutation(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	for _, prefix := range []string{
		"CREATE INDEX ", "CREATE UNIQUE INDEX ", "DROP INDEX ",
		"REINDEX ", "ALTER TABLE ",
	} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func (e *Executor) notifyPostDDL(ctx context.Context, sql string) {
	if !isDDLMutation(sql) {
		return
	}
	e.postDDLMu.RLock()
	hook := e.postDDLHook
	e.postDDLMu.RUnlock()
	if hook == nil {
		return
	}
	if err := hook(ctx); err != nil {
		e.logFn("executor", "post-DDL schema guard failed: %v", err)
	}
}

// dispatchEvent sends a notification event if the dispatcher is set.
// Errors are logged but do not interrupt the executor flow.
func (e *Executor) dispatchEvent(
	ctx context.Context, event notify.Event,
) {
	if e.dispatcher == nil {
		return
	}
	if err := e.dispatcher.Dispatch(ctx, event); err != nil {
		e.logFn("executor",
			"notification dispatch failed: %v", err)
	}
}
