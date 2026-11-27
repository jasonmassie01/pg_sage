// Package gameday runs Sage SRE game days (AI-SRE-SPEC §4 R3): the
// PGIncidentBench fault programs on a disposable clone of a customer
// database, never on a monitored one. Results are ingested as game-day
// evidence for the earned-autonomy ledger (per-family calibration and
// the L3 Safe Pass requirement); a forbidden action on a clone is a
// family safety violation.
package gameday

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Errors.
var (
	ErrNotConfigured = errors.New("game days are not configured")
	ErrRunning       = errors.New("a game day is already running")
	ErrMonitoredDSN  = errors.New("the game-day database is a monitored database")
	// ErrNotDisposable: the local game-day database is not provably
	// disposable (a standby, has standbys, unmarked, or unreadable).
	ErrNotDisposable = errors.New("the game-day database is not provably disposable")
)

// Game-day statuses.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	// StatusCompletedUnscored is a run that finished but scored no run
	// (benches-08): every fault program skipped, or the report had none.
	StatusCompletedUnscored = "completed_unscored"
	StatusFailed            = "failed"
	StatusDestroyFailed     = "destroy_failed"
)

// FaultRunner runs fault programs against a disposable database and
// returns the PGIncidentBench JSON report. bindings are the customer
// programs to run with their tables or skip reasons (benches-12; nil: the
// bench's fixtures only).
type FaultRunner interface {
	Run(ctx context.Context, dsn string, families []string, bindings []Binding) ([]byte,
		error)
}

// Ledger is where game-day evidence goes (earned.Service).
type Ledger interface {
	IngestEvalRun(ctx context.Context, raw []byte, source, actor,
		database string) (earned.EvalRun, error)
	RecordOutcome(ctx context.Context, o earned.Outcome) error
}

// GameDay is one game day. The clone's DSN carries credentials and is
// never kept.
type GameDay struct {
	ID         string     `json:"id"`
	Database   string     `json:"database"`
	Provider   string     `json:"provider"`
	CloneID    string     `json:"clone_id,omitempty"`
	Families   []string   `json:"families"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	EvalRunID  string     `json:"eval_run_id,omitempty"`
	// Summary is what the run's report scored; nil when there was no
	// report (the run failed before it, or the row predates benches-08).
	Summary *Summary `json:"summary,omitempty"`
	// Exercised is what the run exercised; nil for a row that predates
	// benches-12.
	Exercised *Exercised `json:"exercised,omitempty"`
}

// Config configures one database's game days.
type Config struct {
	Database string
	// Provider names the clone provider (dle, snapshot, local).
	Provider string
	// Families to exercise; empty means every family the bench covers.
	Families []string
	Interval time.Duration
	Now      func() time.Time
	// Log reports a background game day that failed (it is also recorded).
	Log func(format string, args ...any)
	// Warn reports a game day that scored nothing (nil: the standard
	// logger).
	Warn func(format string, args ...any)
	// Leases are the shared targets held in this process (nil: the
	// process-wide table every runtime shares).
	Leases *Leases
	// CustomerPrograms runs the customer programs on the clone (nil: when
	// the provider's clones hold customer objects).
	CustomerPrograms *bool
	// Snapshots is the monitored database's collector, which the customer
	// programs' tables are chosen from (nil: none, so they skip).
	Snapshots TableSnapshots
}

// Runner runs one database's game days, one at a time.
type Runner struct {
	cfg      Config
	provider clone.Provider
	faults   FaultRunner
	ledger   Ledger
	store    *Store
	running  atomic.Bool
}

var familyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// NewRunner builds a runner; without a clone provider game days are off.
func NewRunner(cfg Config, provider clone.Provider, faults FaultRunner, ledger Ledger,
	store *Store) (*Runner, error) {
	switch {
	case provider == nil:
		return nil, ErrNotConfigured
	case strings.TrimSpace(cfg.Database) == "":
		return nil, errors.New("game day runner needs a database name")
	case faults == nil || ledger == nil || store == nil:
		return nil, errors.New("game day runner needs fault programs, a ledger and a store")
	}
	for _, f := range cfg.Families {
		if !familyPattern.MatchString(f) {
			return nil, fmt.Errorf("game day family %q is not a family name", f)
		}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 7 * 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = log.Printf
	}
	if cfg.Warn == nil {
		cfg.Warn = defaultWarn
	}
	cfg.Leases = leasesOr(cfg.Leases)
	return &Runner{cfg: cfg, provider: provider, faults: faults, ledger: ledger,
		store: store}, nil
}

func (r *Runner) now() time.Time { return r.cfg.Now().UTC() }

// Run runs one game day now and waits for it. A target another run
// holds refuses it (ErrRunning) before anything is recorded.
func (r *Runner) Run(ctx context.Context) (GameDay, error) {
	if !r.running.CompareAndSwap(false, true) {
		return GameDay{}, ErrRunning
	}
	defer r.running.Store(false)
	id, release, err := r.lease(ctx)
	if err != nil {
		return GameDay{}, err
	}
	defer release()
	return r.run(ctx, id)
}

// Start runs one game day in the background (it takes minutes); a busy
// target refuses it here, not in the background.
func (r *Runner) Start(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	id, release, err := r.lease(ctx)
	if err != nil {
		r.running.Store(false)
		return err
	}
	go func() {
		defer r.running.Store(false)
		defer release()
		if gd, err := r.run(context.WithoutCancel(ctx), id); err != nil {
			r.cfg.Log("gameday: %s on %s %s: %v", gd.ID, gd.Database, gd.Status, err)
		}
	}()
	return nil
}

// lease names the next game day and holds its target for it.
func (r *Runner) lease(ctx context.Context) (string, func(), error) {
	id := newID()
	release, err := r.cfg.Leases.Acquire(ctx, r.provider, Holder{Kind: kindGameDay, ID: id,
		Database: r.cfg.Database})
	return id, release, err
}

// RunDue runs a game day when the last one started an interval ago.
func (r *Runner) RunDue(ctx context.Context) (GameDay, bool, error) {
	last, err := r.store.Latest(ctx, r.cfg.Database)
	if err != nil {
		return GameDay{}, false, err
	}
	if last != nil && r.now().Sub(last.StartedAt) < r.cfg.Interval {
		return GameDay{}, false, nil
	}
	gd, err := r.Run(ctx)
	return gd, true, err
}

// List lists the database's game days, newest first.
func (r *Runner) List(ctx context.Context, limit int) ([]GameDay, error) {
	return r.store.List(ctx, r.cfg.Database, limit)
}

func (r *Runner) run(ctx context.Context, id string) (GameDay, error) {
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()
	bindings := r.bindings()
	planned := exercisedOf(r.cfg.Provider, bindings, nil)
	gd := GameDay{ID: id, Database: r.cfg.Database, Provider: r.cfg.Provider,
		Families: append([]string{}, r.cfg.Families...), Status: StatusRunning,
		StartedAt: r.now(), Exercised: &planned}
	if err := r.store.start(ctx, gd, rowGameDay); err != nil {
		return gd, err
	}
	c, err := r.provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return r.finish(ctx, gd, StatusFailed, fmt.Errorf("create clone: %w", err))
	}
	gd.CloneID = c.ID
	runErr := r.store.attachClone(ctx, gd.ID, c.ID)
	if runErr == nil {
		runErr = r.exercise(ctx, &gd, c.DSN, bindings)
	}
	status, runErr := destroyClone(ctx, r.provider, c, runErr)
	return r.finish(ctx, gd, r.scoredStatus(status, gd), runErr)
}

// scoredStatus is a finished game day's status: completed_unscored when
// its report scored nothing (benches-08) or a customer program it ran
// scored nothing (benches-12: the run then says nothing about this
// database); each is logged once.
func (r *Runner) scoredStatus(status string, gd GameDay) string {
	if status = scoredStatus(status, gd.Summary); status == StatusCompletedUnscored {
		warnUnscored(r.cfg.Warn, "game day", gd.ID, gd.Database, gd.Summary)
		return status
	}
	if status != StatusCompleted || gd.Exercised == nil {
		return status
	}
	if sc, unscored := customerUnscored(*gd.Exercised); unscored {
		r.cfg.Warn("game day %s on %s: completed_unscored: customer program %s scored no run: %s",
			gd.ID, gd.Database, sc.ID, sc.Skip)
		return StatusCompletedUnscored
	}
	return status
}

// bindings are the customer programs' tables, or why each has none.
func (r *Runner) bindings() []Binding {
	b := Binding{Program: ProgramCustomerHotTable}
	on := HoldsCustomerObjects(r.cfg.Provider)
	if r.cfg.CustomerPrograms != nil {
		on = *r.cfg.CustomerPrograms
	}
	switch {
	case !HoldsCustomerObjects(r.cfg.Provider):
		b.Skip = SkipLocalProvider
	case !on:
		b.Skip = SkipProgramsOff
	case r.cfg.Snapshots == nil:
		b.Skip = SkipNoSnapshot
	default:
		b = HotTableBinding(r.cfg.Snapshots.LatestSnapshot(), r.cfg.Snapshots.PreviousSnapshot())
	}
	return []Binding{b}
}

// destroyClone destroys a run's clone, even when ctx is cancelled, and
// returns the run's status: completed, failed (runErr), or
// destroy_failed.
func destroyClone(ctx context.Context, provider clone.Provider, c clone.Clone,
	runErr error) (string, error) {
	status := StatusCompleted
	if runErr != nil {
		status = StatusFailed
	}
	if err := provider.Destroy(context.WithoutCancel(ctx), c); err != nil {
		status = StatusDestroyFailed
		runErr = errors.Join(runErr, fmt.Errorf("destroy clone %s: %w", c.ID, err))
	}
	return status, runErr
}

// exercise runs the fault programs on the clone and records the evidence.
// A customer program that errored fails the game day after its report is
// ingested (its errored run is excluded from every rate).
func (r *Runner) exercise(ctx context.Context, gd *GameDay, dsn string,
	bindings []Binding) error {
	raw, err := r.faults.Run(ctx, dsn, gd.Families, bindings)
	if err != nil {
		return fmt.Errorf("fault programs: %w", err)
	}
	summary := summarize(raw)
	exercised := exercisedOf(r.cfg.Provider, bindings, raw)
	gd.Summary, gd.Exercised = &summary, &exercised
	run, err := r.ledger.IngestEvalRun(ctx, raw, earned.SourceGameDay, earned.ActorPgSage,
		gd.Database)
	if err != nil {
		return fmt.Errorf("ingest game-day report: %w", err)
	}
	gd.EvalRunID = run.ID
	return errors.Join(r.recordViolations(ctx, run), customerFailure(exercised))
}

// recordViolations turns each family's forbidden actions into a safety
// violation, which demotes that family.
func (r *Runner) recordViolations(ctx context.Context, run earned.EvalRun) error {
	var errs []error
	for _, c := range run.Cells {
		if c.Forbidden == 0 || !earned.KnownFamily(earned.Family(c.Family)) {
			continue
		}
		errs = append(errs, r.ledger.RecordOutcome(ctx, earned.Outcome{
			Database: r.cfg.Database, Family: earned.Family(c.Family),
			Class: earned.ClassUnclassified, Level: earned.L0,
			Result: earned.ResultSafetyViolation, Source: earned.SourceGameDay,
			Actor: earned.ActorPgSage, Detail: fmt.Sprintf(
				"game day %s: %d forbidden actions by arm %s", run.ID, c.Forbidden, c.Arm)}))
	}
	return errors.Join(errs...)
}

func (r *Runner) finish(ctx context.Context, gd GameDay, status string,
	runErr error) (GameDay, error) {
	at := r.now()
	gd.Status, gd.FinishedAt = status, &at
	if runErr != nil {
		gd.Error = truncate(runErr.Error(), 2000)
	}
	if err := r.store.finish(context.WithoutCancel(ctx), gd); err != nil {
		return gd, errors.Join(runErr, err)
	}
	return gd, runErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
