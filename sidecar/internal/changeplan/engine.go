package changeplan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/store"
)

// The plan engine (plan-02a) runs approved plans one step at a time. Each
// tick advances each approved or running plan by at most one engine
// decision: judge a step, record its verdict, skip, complete, halt, void,
// or start the next step. Starting a step is one decision that walks the
// approval path in order: starting, the queue item (queued), its approval
// as the plan's approver with exactly the step's SQL, and the executor's
// RunApprovedAction, which routes it to this engine's runner (running,
// then executed). Every change still goes through Apply and the gate.

// StepExecutor is the executor surface steps run through
// (*executor.Executor).
type StepExecutor interface {
	RunApprovedAction(ctx context.Context, action store.QueuedAction,
		approvedBy int) (executor.ApprovedRun, error)
	RollbackAction(ctx context.Context, actionID int64, reason string) error
}

// InFlightWaits is the executor's one-change-per-object lookup
// (*executor.VerificationWaits).
type InFlightWaits interface {
	PendingFor(ctx context.Context, sql string,
		targets []string) ([]policy.PendingVerification, error)
	AnyInFlight(ctx context.Context) (bool, error)
}

// ManualExecutor runs one approved change through Apply
// (*executor.Executor's ExecuteManual).
type ManualExecutor interface {
	ExecuteManual(ctx context.Context, findingID int, sql, rollbackSQL string,
		approvedBy *int) (int64, error)
}

// ActionVerdict is what the verifier recorded for an action: its stored
// outcome verdict ("" or "pending" while undecided, decided at DecidedAt)
// and the action's own outcome (rolled_back, verification_expired, ...).
type ActionVerdict struct {
	Verdict       string
	DecidedAt     *time.Time
	ActionOutcome string
	// Detail is what an operator step's judge says about how it judged
	// (recorded with the verdict event); nil for an action's verdict.
	Detail map[string]any
}

// VerdictSource reads an action's verdict (DBVerdicts in production).
type VerdictSource interface {
	Verdict(ctx context.Context, actionLogID int64) (ActionVerdict, error)
}

// Dispatcher sends a notification (the database's Notifications
// channels).
type Dispatcher interface {
	Dispatch(ctx context.Context, evt notify.Event) error
}

// EngineConfig wires an engine to one monitored database.
type EngineConfig struct {
	Pool     *pgxpool.Pool
	Executor StepExecutor
	Waits    InFlightWaits
	Verdicts VerdictSource
	Notify   Dispatcher // nil: no notification
	// Leader reports fleet leadership; only the leader ticks (nil: always).
	Leader func() bool
	// Stopped reads the emergency stop (nil: executor.CheckEmergencyStop).
	Stopped  func(context.Context) bool
	Now      func() time.Time // nil: time.Now
	Owner    string           // this engine's claim name (one per sidecar)
	Database string
	Lease    time.Duration // a step claim's life (0: DefaultClaimLease)
	LogFn    func(string, string, ...any)
	// Caps reads the plan caps (nil: the policy defaults); Sizes reads
	// the live sizes the byte cap needs (nil: DBSizes on Pool).
	Caps  CapsSource
	Sizes SizeReader
	// Operator judges landed operator steps (nil: NewOperatorJudge(nil,
	// 0), which decides readback steps only); Landings reads their
	// landing (nil: DBLandings on Pool) (plan-02d).
	Operator OperatorJudge
	Landings LandingReader
}

// DefaultClaimLease is how long an engine's claim on a step holds.
const DefaultClaimLease = 15 * time.Minute

// maxActivePlans bounds the plans one tick reads.
const maxActivePlans = 500

// planEngineLock serializes the engines' decisions on one database, so two
// sidecars never start conflicting steps of different plans at once.
const planEngineLock int64 = 0x7067_5f70_6c61_6e30

// pauseEmergencyStop is a running plan's pause reason under the stop.
const pauseEmergencyStop = "emergency stop"

// Engine errors.
var (
	ErrIllegalTransition = errors.New("changeplan: illegal step transition")
	ErrStepMoved         = errors.New("changeplan: the step is no longer in that state")
	// ErrNotEngineRun refuses a plan step's queue item that its engine did
	// not hand to the executor (an Actions approval, a card, a schedule).
	ErrNotEngineRun = errors.New("changeplan: a plan step runs only when its plan " +
		"engine starts it")
)

// Engine advances one database's plans.
type Engine struct {
	cfg     EngineConfig
	actions *store.ActionStore
	resume  resumeReader
	// needResume is set until a leading tick has resumed every step a
	// restart or a leader change left in flight (plan-02e).
	mu         sync.Mutex
	needResume bool
}

// NewEngine checks cfg and returns its engine.
func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.Pool == nil || cfg.Executor == nil || cfg.Waits == nil || cfg.Verdicts == nil {
		return nil, errors.New("changeplan: an engine needs a pool, an executor, the " +
			"in-flight lookup and a verdict source")
	}
	if cfg.Owner == "" {
		return nil, errors.New("changeplan: an engine needs an owner name for its claims")
	}
	if cfg.Leader == nil {
		cfg.Leader = func() bool { return true }
	}
	if cfg.Stopped == nil {
		pool := cfg.Pool
		cfg.Stopped = func(ctx context.Context) bool {
			return executor.CheckEmergencyStop(ctx, pool)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Lease <= 0 {
		cfg.Lease = DefaultClaimLease
	}
	if cfg.LogFn == nil {
		cfg.LogFn = func(string, string, ...any) {}
	}
	if cfg.Caps == nil {
		cfg.Caps = noPolicyCaps
	}
	if cfg.Sizes == nil {
		cfg.Sizes = NewDBSizes(cfg.Pool)
	}
	if cfg.Operator == nil {
		cfg.Operator = NewOperatorJudge(nil, 0)
	}
	if cfg.Landings == nil {
		cfg.Landings = NewDBLandings(cfg.Pool)
	}
	return &Engine{cfg: cfg, actions: store.NewActionStore(cfg.Pool),
		resume: dbResume{pool: cfg.Pool}, needResume: true}, nil
}

// Tick advances each approved or running plan by at most one decision. A
// follower does nothing. The first leading tick (after a start or a
// leader change) resumes what was left in flight first, and a plan whose
// resume is not settled (another engine's live claim, a failed read)
// waits for the next tick (plan-02e). While the emergency stop is set
// nothing moves and each running plan shows "emergency stop". It returns
// the plans' engine failures (a step's own failure is recorded on the
// step, not returned).
func (e *Engine) Tick(ctx context.Context) error {
	if !e.cfg.Leader() {
		e.setNeedResume(true)
		return nil
	}
	ids, err := e.activePlans(ctx)
	if err != nil {
		return err
	}
	stopped := e.cfg.Stopped(ctx)
	var errs []error
	unsettled := map[int64]bool{}
	if !stopped && e.needsResume() {
		var resumeErr error
		_, unsettled, resumeErr = e.resumeAll(ctx)
		if resumeErr != nil {
			errs = append(errs, resumeErr)
		}
		e.setNeedResume(len(unsettled) > 0 || resumeErr != nil)
	}
	for _, id := range ids {
		if unsettled[id] {
			continue
		}
		if err := e.advance(ctx, id, stopped); err != nil {
			e.cfg.LogFn("plan", "plan %d: %v", id, err)
			errs = append(errs, fmt.Errorf("plan %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) activePlans(ctx context.Context) ([]int64, error) {
	rows, err := e.cfg.Pool.Query(ctx, `/* pg_sage */ SELECT id FROM sage.plan
		WHERE status IN ('approved', 'running') ORDER BY id LIMIT $1`, maxActivePlans)
	if err != nil {
		return nil, fmt.Errorf("changeplan: list the plans to advance: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("changeplan: list the plans to advance: %w", err)
	}
	return ids, nil
}

// followUp is work a decision leaves for after its transaction commits
// (running a step, rolling one back, a notification).
type followUp func(context.Context) error

// advance makes plan planID's next decision under the engines' lock and
// the plan row's lock; a plan another engine holds is skipped this tick.
func (e *Engine) advance(ctx context.Context, planID int64, stopped bool) error {
	var next followUp
	err := e.inTx(ctx, func(tx pgx.Tx) error {
		p, ok, err := lockForTick(ctx, tx, planID)
		if err != nil || !ok {
			return err
		}
		pause := ""
		if stopped {
			pause = pauseEmergencyStop
		}
		if err := setPause(ctx, tx, p, pause); err != nil || stopped {
			return err
		}
		next, err = e.decideAndApply(ctx, tx, p)
		return err
	})
	if err != nil || next == nil {
		return err
	}
	return next(ctx)
}

// lockForTick takes the engines' lock and plan planID's row without
// waiting: false when another engine holds either, or the plan moved on.
func lockForTick(ctx context.Context, tx pgx.Tx, planID int64) (Plan, bool, error) {
	var got bool
	if err := tx.QueryRow(ctx, `/* pg_sage */ SELECT pg_try_advisory_xact_lock($1)`,
		planEngineLock).Scan(&got); err != nil || !got {
		return Plan{}, false, err
	}
	var id int64
	err := tx.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.plan WHERE id = $1
		AND status IN ('approved', 'running') FOR UPDATE SKIP LOCKED`, planID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, err
	}
	p, err := loadPlan(ctx, tx, planID)
	return p, err == nil, err
}

func setPause(ctx context.Context, tx pgx.Tx, p Plan, reason string) error {
	if p.PauseReason == reason {
		return nil
	}
	_, err := tx.Exec(ctx, `/* pg_sage */ UPDATE sage.plan SET pause_reason = $2
		WHERE id = $1`, p.ID, reason)
	if err != nil {
		return fmt.Errorf("record the pause: %w", err)
	}
	return nil
}

// inTx runs fn in one transaction bounded by lock and statement timeouts.
func (e *Engine) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return (&Store{pool: e.cfg.Pool}).inTx(ctx, fn)
}
