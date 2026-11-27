package executor

import "context"

// remnantHooks are the remnant handler's test seams.
type remnantHooks struct {
	beforeDrop func(context.Context) error
}

// remnantRunFor runs sql under its class timeout; each cleanup DROP runs
// under safety.ddl_timeout_seconds and the same lock timeout.
func (e *Executor) remnantRunFor(sql string, opts ...DDLOption) remnantRun {
	return remnantRun{pool: e.actionDB(), logFn: e.logFn, timeout: e.ddlTimeoutFor(sql),
		dropTimeout: e.ddlTimeout(), opts: opts, beforeDrop: e.remnantHooks.beforeDrop}
}

// execConcurrentDDL runs a top-level statement through the remnant handler
// and records a cleanup as remnant_cleanup in state, the action row's
// before_state (nil records nothing; the error still names the cleanup).
// A VACUUM (not VACUUM FULL) runs watched instead (vacuum-05): it leaves no
// remnant, and its statement_timeout is 0.
func (e *Executor) execConcurrentDDL(
	ctx context.Context, sql string, state map[string]any, opts ...DDLOption,
) error {
	if isWatchedVacuum(sql) {
		return e.execVacuumWatched(ctx, sql, state, opts...)
	}
	cleanup, err := e.remnantRunFor(sql, opts...).exec(ctx, sql)
	recordRemnantCleanup(state, cleanup)
	return err
}
