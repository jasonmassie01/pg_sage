package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// Outcomes of an executor VACUUM its watchdog ended (vacuum-05).
const (
	VacuumStalled       = "vacuum_stalled"
	VacuumCap           = freeze.VacuumCapOutcome
	VacuumEmergencyStop = "emergency_stop"
)

const (
	// vacuumWatchInterval is how often the watchdog samples progress.
	vacuumWatchInterval = 30 * time.Second
	// vacuumCancelTimeout bounds each cancel, emergency-stop read and
	// progress probe.
	vacuumCancelTimeout = 5 * time.Second
	// vacuumAbortGrace is how long after the cap the session itself is
	// closed if the cancel never took effect.
	vacuumAbortGrace = 30 * time.Second
	// vacuumCostDelayBeforeKey is the before-state key of the
	// vacuum_cost_delay an emergency VACUUM replaced with 0 (vacuum-05b).
	vacuumCostDelayBeforeKey = "vacuum_cost_delay_before"
)

// vacuumHooks are the watched VACUUM's test seams; zero values use the
// configured windows and the real probes.
type vacuumHooks struct {
	interval      time.Duration
	stall         time.Duration
	cap           time.Duration
	readProgress  vacuumProgressReader
	cancelBackend func(ctx context.Context, pid uint32) error
	emergencyStop func(ctx context.Context) (bool, error)
	onSession     func(ctx context.Context, conn *pgxpool.Conn) error
}

// VacuumWatchError reports a VACUUM the watchdog cancelled: why, the
// backend, its last progress, a refused cancel (the session was then
// closed) and the statement's own error.
type VacuumWatchError struct {
	Outcome   string
	PID       uint32
	Progress  vacuumProgress
	CancelErr error
	Err       error
}

func (e *VacuumWatchError) Error() string {
	msg := fmt.Sprintf("%s: the watchdog cancelled VACUUM backend %d (%s)",
		e.Outcome, e.PID, e.Progress)
	if e.CancelErr != nil {
		msg += fmt.Sprintf("; the cancel was refused (%v), so its session was closed",
			e.CancelErr)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *VacuumWatchError) Unwrap() []error {
	var errs []error
	for _, err := range []error{e.Err, e.CancelErr} {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if e.Outcome == VacuumEmergencyStop {
		errs = append(errs, ErrEmergencyStop)
	}
	return errs
}

// isWatchedVacuum reports a VACUUM that is not VACUUM FULL.
func isWatchedVacuum(sql string) bool { return timeoutClassOf(sql) == classVacuum }

// execVacuumWatched runs a VACUUM on a dedicated session with
// statement_timeout 0 and the caller's lock_timeout: a freeze of a large
// table advances relfrozenxid only when its scan completes, so a statement
// timeout only ever wasted it. The watchdog bounds it instead. An
// emergency VACUUM (freeze.IsEmergencyVacuumSQL) also runs with
// vacuum_cost_delay 0; the value it replaced goes into state.
func (e *Executor) execVacuumWatched(
	ctx context.Context, sql string, state map[string]any, opts ...DDLOption,
) error {
	if err := ValidateExecutorSQL(sql); err != nil {
		return fmt.Errorf("SQL validation: %w", err)
	}
	if e.actionDB() == nil {
		return errors.New("execute VACUUM: database pool unavailable")
	}
	w := e.newVacuumWatch()
	o := applyDDLOpts(opts)
	o.resetCostDelay = freeze.IsEmergencyVacuumSQL(sql)
	err := withTopLevelSession(ctx, e.actionDB(), sql, 0, o,
		func(conn pooledExecutor) error { return w.run(ctx, conn, sql) })
	if w.costDelayBefore != "" && state != nil {
		state[vacuumCostDelayBeforeKey] = w.costDelayBefore
	}
	return stoppedStatement(ctx, sql, err)
}

type vacuumWatch struct {
	interval, stall, cap time.Duration
	read                 vacuumProgressReader
	cancel               func(ctx context.Context, pid uint32) error
	stopped              func(ctx context.Context) (bool, error)
	onSession            func(ctx context.Context, conn *pgxpool.Conn) error
	logFn                func(string, string, ...any)
	// costDelayBefore is the vacuum_cost_delay an emergency VACUUM's
	// session replaced with 0 (empty when it replaced none).
	costDelayBefore string
}

func (e *Executor) newVacuumWatch() *vacuumWatch {
	h, safety := e.vacuumHooks, e.safetySnapshot()
	w := &vacuumWatch{interval: h.interval, stall: h.stall, cap: h.cap,
		read: h.readProgress, cancel: h.cancelBackend, stopped: h.emergencyStop,
		onSession: h.onSession, logFn: e.logFn}
	if w.interval <= 0 {
		w.interval = vacuumWatchInterval
	}
	if w.stall <= 0 {
		w.stall = safety.VacuumStall()
	}
	if w.cap <= 0 {
		w.cap = safety.VacuumTimeout()
	}
	if w.read == nil {
		w.read = e.readVacuumProgress
	}
	if w.cancel == nil {
		w.cancel = e.cancelVacuumBackend
	}
	if w.stopped == nil {
		w.stopped = e.readEmergencyStopFlag
	}
	return w
}

// run executes sql while the watchdog samples its backend.
func (w *vacuumWatch) run(ctx context.Context, conn pooledExecutor, sql string) error {
	ps, ok := conn.(pooledSession)
	if !ok {
		return errors.New("execute VACUUM: a watched VACUUM needs a pooled session")
	}
	if w.onSession != nil {
		if err := w.onSession(ctx, ps.Conn); err != nil {
			return fmt.Errorf("prepare VACUUM session: %w", err)
		}
	}
	if freeze.IsEmergencyVacuumSQL(sql) {
		if err := w.dropCostDelay(ctx, ps); err != nil {
			return err
		}
	}
	pid := ps.Conn.Conn().PgConn().PID()
	execCtx, abort := context.WithTimeout(ctx, w.cap+vacuumAbortGrace)
	defer abort()
	watchCtx, stopWatch := context.WithCancel(ctx)
	verdicts := make(chan *VacuumWatchError, 1)
	go func() { verdicts <- w.watch(watchCtx, pid, abort) }()
	_, execErr := ps.Exec(execCtx, sql)
	stopWatch()
	verdict := <-verdicts
	if execErr == nil {
		return nil // it completed before a cancel took effect
	}
	if verdict == nil {
		return wrapDDLError(execErr)
	}
	verdict.Err = wrapDDLError(execErr)
	return verdict
}

// dropCostDelay runs an emergency VACUUM unthrottled: a role or database
// setting can raise vacuum_cost_delay above the manual VACUUM default of 0.
// The session's reset (ddlOpts.resetCostDelay) restores it.
func (w *vacuumWatch) dropCostDelay(ctx context.Context, ps pooledSession) error {
	var before string
	if err := ps.QueryRow(ctx, "/* pg_sage executor */ SELECT "+
		"pg_catalog.current_setting('vacuum_cost_delay')").Scan(&before); err != nil {
		return fmt.Errorf("prepare emergency VACUUM: read vacuum_cost_delay: %w", err)
	}
	if _, err := ps.Exec(ctx, "SET vacuum_cost_delay = 0"); err != nil {
		return fmt.Errorf("prepare emergency VACUUM: SET vacuum_cost_delay = 0: %w", err)
	}
	w.costDelayBefore = before
	return nil
}

// watch samples the backend every interval until ctx ends, and cancels it
// at the cap, on an emergency stop, or on a stalled heap phase. A failed
// probe never cancels: the cap still bounds the run.
func (w *vacuumWatch) watch(ctx context.Context, pid uint32, abort func()) *VacuumWatchError {
	capTimer := time.NewTimer(w.cap)
	defer capTimer.Stop()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	tracker := stallTracker{stall: w.stall}
	var last vacuumProgress
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-capTimer.C:
			return w.end(ctx, pid, VacuumCap, last, abort)
		case <-ticker.C:
		}
		if fired(capTimer) {
			return w.end(ctx, pid, VacuumCap, last, abort)
		}
		if w.emergencyStopped(ctx) {
			return w.end(ctx, pid, VacuumEmergencyStop, last, abort)
		}
		p, ok, err := w.sample(ctx, pid)
		if err != nil || !ok {
			if err != nil && ctx.Err() == nil {
				w.logFn("executor", "VACUUM watchdog probe of pid %d: %v", pid, err)
			}
			tracker.reset()
			continue
		}
		last = p
		if tracker.observe(p, time.Now()) {
			return w.end(ctx, pid, VacuumStalled, p, abort)
		}
	}
}

// fired reports a timer that has fired. select picks at random among ready
// cases, so a tick ready with the cap must not run another probe first.
func fired(t *time.Timer) bool {
	select {
	case <-t.C:
		return true
	default:
		return false
	}
}

// sample reads the backend's progress under vacuumCancelTimeout: a probe
// stuck acquiring a connection or on the network must not keep the
// watchdog from the cap or an emergency stop.
func (w *vacuumWatch) sample(ctx context.Context, pid uint32) (vacuumProgress, bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, vacuumCancelTimeout)
	defer cancel()
	return w.read(readCtx, pid)
}

// emergencyStopped reports this run's in-process stop or the durable flag
// another instance may have set. A flag that cannot be read does not
// cancel a run that may have taken hours: the in-process stop already
// cancels its statement directly.
func (w *vacuumWatch) emergencyStopped(ctx context.Context) bool {
	if run := runFromContext(ctx); run != nil && run.stopCause() != nil {
		return true
	}
	readCtx, cancel := context.WithTimeout(ctx, vacuumCancelTimeout)
	defer cancel()
	stopped, err := w.stopped(readCtx)
	if err != nil && ctx.Err() == nil {
		w.logFn("executor", "VACUUM watchdog emergency-stop read: %v", err)
	}
	return err == nil && stopped
}

// end cancels the backend; a refused cancel closes the session instead.
func (w *vacuumWatch) end(
	ctx context.Context, pid uint32, outcome string, p vacuumProgress, abort func(),
) *VacuumWatchError {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vacuumCancelTimeout)
	defer cancel()
	verdict := &VacuumWatchError{Outcome: outcome, PID: pid, Progress: p}
	if err := w.cancel(cancelCtx, pid); err != nil {
		verdict.CancelErr = err
		w.logFn("executor", "%s: cancel of VACUUM pid %d refused, closing its session: %v",
			outcome, pid, err)
		abort()
	}
	return verdict
}

// cancelVacuumBackend cancels pg_sage's own VACUUM backend: a role may
// signal its own sessions on every provider without a grant.
func (e *Executor) cancelVacuumBackend(ctx context.Context, pid uint32) error {
	var cancelled bool
	if err := e.actionDB().QueryRow(ctx, "/* pg_sage executor */ SELECT pg_cancel_backend($1)",
		int32(pid)).Scan(&cancelled); err != nil {
		return fmt.Errorf("pg_cancel_backend(%d): %w", pid, err)
	}
	if !cancelled {
		return fmt.Errorf("pg_cancel_backend(%d) returned false: the backend is gone or "+
			"cannot be signalled", pid)
	}
	return nil
}

func (e *Executor) readEmergencyStopFlag(ctx context.Context) (bool, error) {
	state, err := ReadEmergencyStop(ctx, e.pool)
	return state.Stopped, err
}
