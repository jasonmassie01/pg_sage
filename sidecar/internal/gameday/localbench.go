package gameday

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/earned"
)

// Local bench runs (roadmap 1.1, 2026-10-03): "Run bench locally" runs
// the PGIncidentBench fault programs on a disposable clone of the
// database, like a game day, but ingests the report as bench evidence
// marked "local run". It counts only for the families it covered and
// only for the running pg_sage build.

// ErrUnknownFamily is a requested family that is not a shipped one.
var ErrUnknownFamily = errors.New("not a shipped incident family")

// BenchLedger is where local-run reports go (earned.Service).
type BenchLedger interface {
	IngestBench(ctx context.Context, raw []byte, in earned.BenchIngest) (earned.EvalRun,
		error)
}

// LocalBenchConfig configures one database's local bench runs.
type LocalBenchConfig struct {
	Database string
	// Provider names the clone provider (dle, snapshot, local).
	Provider string
	Now      func() time.Time
	// Log reports a background run that failed (it is also kept as Last).
	Log func(format string, args ...any)
	// Warn reports a run that scored nothing (nil: the standard logger).
	Warn func(format string, args ...any)
	// Leases are the shared targets held in this process (nil: the
	// process-wide table game days share too).
	Leases *Leases
	// Store records runs as kind 'local' (benches-03), so a run its
	// process never finished is abandoned and its clone destroyed. Nil
	// keeps runs in memory only.
	Store *Store
}

// LocalBenchRun is one local bench run. The clone's DSN carries
// credentials and is never kept.
type LocalBenchRun struct {
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
	// Summary is what the run's report scored, as for a game day.
	Summary *Summary `json:"summary,omitempty"`
	// Exercised mirrors GameDay's (the two convert); a local bench run
	// binds no customer table, so it stays nil.
	Exercised *Exercised `json:"exercised,omitempty"`
}

// LocalBench runs one database's local bench runs, one at a time.
type LocalBench struct {
	cfg      LocalBenchConfig
	provider clone.Provider
	faults   FaultRunner
	ledger   BenchLedger
	running  atomic.Bool

	mu   sync.Mutex
	last *LocalBenchRun
}

// NewLocalBench builds a database's local bench; without a clone
// provider there is none (ErrNotConfigured).
func NewLocalBench(cfg LocalBenchConfig, provider clone.Provider, faults FaultRunner,
	ledger BenchLedger) (*LocalBench, error) {
	switch {
	case provider == nil:
		return nil, ErrNotConfigured
	case strings.TrimSpace(cfg.Database) == "":
		return nil, errors.New("local bench needs a database name")
	case faults == nil || ledger == nil:
		return nil, errors.New("local bench needs fault programs and a ledger")
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
	return &LocalBench{cfg: cfg, provider: provider, faults: faults, ledger: ledger}, nil
}

// Provider names the clone provider runs use.
func (b *LocalBench) Provider() string { return b.cfg.Provider }

// Running reports whether a run is in progress.
func (b *LocalBench) Running() bool { return b.running.Load() }

// Last is the newest run (in progress or finished), if any.
func (b *LocalBench) Last() (LocalBenchRun, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.last == nil {
		return LocalBenchRun{}, false
	}
	run := *b.last
	run.Families = append([]string{}, run.Families...)
	return run, true
}

// Run runs the fault programs of families (empty: every family) now and
// waits for it.
func (b *LocalBench) Run(ctx context.Context, families []string) (LocalBenchRun, error) {
	run, release, err := b.begin(ctx, families)
	if err != nil {
		return LocalBenchRun{}, err
	}
	defer b.running.Store(false)
	defer release()
	return b.execute(ctx, run)
}

// Start runs in the background (a run takes minutes) and returns the
// started run; the run outlives ctx's cancellation.
func (b *LocalBench) Start(ctx context.Context, families []string) (LocalBenchRun, error) {
	run, release, err := b.begin(ctx, families)
	if err != nil {
		return LocalBenchRun{}, err
	}
	go func() {
		defer b.running.Store(false)
		defer release()
		if done, err := b.execute(context.WithoutCancel(ctx), run); err != nil {
			b.cfg.Log("local bench %s on %s %s: %v", done.ID, done.Database, done.Status, err)
		}
	}()
	return run, nil
}

// begin validates families, takes the single run slot and the target's
// lease, and records the run as in progress; a refused run is not kept.
func (b *LocalBench) begin(ctx context.Context, families []string) (LocalBenchRun, func(),
	error) {
	for _, f := range families {
		// The bench exercises incident families; a self-initiated trust
		// family (tuning, hygiene) has no fault program.
		if !earned.IsIncidentFamily(earned.Family(f)) {
			return LocalBenchRun{}, nil, fmt.Errorf("%w: %q", ErrUnknownFamily, f)
		}
	}
	if !b.running.CompareAndSwap(false, true) {
		return LocalBenchRun{}, nil, ErrRunning
	}
	id := newID()
	release, err := b.cfg.Leases.Acquire(ctx, b.provider, Holder{Kind: kindLocalBench, ID: id,
		Database: b.cfg.Database})
	if err != nil {
		b.running.Store(false)
		return LocalBenchRun{}, nil, err
	}
	run := LocalBenchRun{ID: id, Database: b.cfg.Database, Provider: b.cfg.Provider,
		Families: append([]string{}, families...), Status: StatusRunning,
		StartedAt: b.cfg.Now().UTC()}
	if err := b.persistStart(ctx, run); err != nil {
		release()
		b.running.Store(false)
		return LocalBenchRun{}, nil, err
	}
	b.record(run)
	return run, release, nil
}

// persistStart sweeps this database's stale local runs (a failed sweep is
// logged; it does not block the run) and records run as running. A run
// that cannot be recorded is refused: its clone could leak unrecorded.
func (b *LocalBench) persistStart(ctx context.Context, run LocalBenchRun) error {
	if b.cfg.Store == nil {
		return nil
	}
	if _, err := b.sweep(ctx); err != nil {
		b.cfg.Log("local bench on %s: sweep stale runs: %v", b.cfg.Database, err)
	}
	return b.cfg.Store.start(ctx, GameDay(run), rowLocal)
}

func (b *LocalBench) execute(ctx context.Context, run LocalBenchRun) (LocalBenchRun,
	error) {
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()
	c, err := b.provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return b.finish(ctx, run, StatusFailed, fmt.Errorf("create clone: %w", err))
	}
	run.CloneID = c.ID
	var runErr error
	if b.cfg.Store != nil {
		runErr = b.cfg.Store.attachClone(ctx, run.ID, c.ID)
	}
	if runErr == nil {
		runErr = b.exercise(ctx, &run, c.DSN)
	}
	status, runErr := destroyClone(ctx, b.provider, c, runErr)
	if status = scoredStatus(status, run.Summary); status == StatusCompletedUnscored {
		warnUnscored(b.cfg.Warn, "local bench", run.ID, run.Database, run.Summary)
	}
	return b.finish(ctx, run, status, runErr)
}

// exercise runs the fault programs on the clone and ingests the report
// as a local run of this pg_sage.
func (b *LocalBench) exercise(ctx context.Context, run *LocalBenchRun, dsn string) error {
	// No bindings: a local bench run measures the bench's fixtures, not
	// the customer's tables (benches-12 binds game days only).
	raw, err := b.faults.Run(ctx, dsn, run.Families, nil)
	if err != nil {
		return fmt.Errorf("fault programs: %w", err)
	}
	summary := summarize(raw)
	run.Summary = &summary
	stored, err := b.ledger.IngestBench(ctx, raw, earned.BenchIngest{
		Origin: earned.OriginLocalRun, Actor: earned.ActorPgSage})
	if err != nil {
		return fmt.Errorf("ingest local bench report: %w", err)
	}
	run.EvalRunID = stored.ID
	return nil
}

func (b *LocalBench) finish(ctx context.Context, run LocalBenchRun, status string,
	runErr error) (LocalBenchRun, error) {
	at := b.cfg.Now().UTC()
	run.Status, run.FinishedAt = status, &at
	if runErr != nil {
		run.Error = truncate(runErr.Error(), 2000)
	}
	b.record(run)
	if b.cfg.Store != nil {
		if err := b.cfg.Store.finish(context.WithoutCancel(ctx), GameDay(run)); err != nil {
			return run, errors.Join(runErr, err)
		}
	}
	return run, runErr
}

func (b *LocalBench) record(run LocalBenchRun) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = &run
}

// BenchRegistry maps fleet database names to their local benches.
type BenchRegistry struct {
	mu      sync.RWMutex
	benches map[string]*LocalBench
}

// NewBenchRegistry is an empty registry.
func NewBenchRegistry() *BenchRegistry {
	return &BenchRegistry{benches: map[string]*LocalBench{}}
}

// Register binds a database's local bench; nil or an empty name is ignored.
func (r *BenchRegistry) Register(database string, b *LocalBench) {
	if b == nil || strings.TrimSpace(database) == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.benches[database] = b
}

// Remove unbinds a database.
func (r *BenchRegistry) Remove(database string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.benches, database)
}

// Lookup resolves a database's local bench.
func (r *BenchRegistry) Lookup(database string) (*LocalBench, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.benches[database]
	return b, ok
}
