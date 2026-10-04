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
	run, err := b.begin(families)
	if err != nil {
		return LocalBenchRun{}, err
	}
	defer b.running.Store(false)
	return b.execute(ctx, run)
}

// Start runs in the background (a run takes minutes) and returns the
// started run; the run outlives ctx's cancellation.
func (b *LocalBench) Start(ctx context.Context, families []string) (LocalBenchRun, error) {
	run, err := b.begin(families)
	if err != nil {
		return LocalBenchRun{}, err
	}
	go func() {
		defer b.running.Store(false)
		if done, err := b.execute(context.WithoutCancel(ctx), run); err != nil {
			b.cfg.Log("local bench %s on %s %s: %v", done.ID, done.Database, done.Status, err)
		}
	}()
	return run, nil
}

// begin validates families, takes the single run slot and records the
// run as in progress.
func (b *LocalBench) begin(families []string) (LocalBenchRun, error) {
	for _, f := range families {
		// The bench exercises incident families; a self-initiated trust
		// family (tuning, hygiene) has no fault program.
		if !earned.IsIncidentFamily(earned.Family(f)) {
			return LocalBenchRun{}, fmt.Errorf("%w: %q", ErrUnknownFamily, f)
		}
	}
	if !b.running.CompareAndSwap(false, true) {
		return LocalBenchRun{}, ErrRunning
	}
	run := LocalBenchRun{ID: newID(), Database: b.cfg.Database, Provider: b.cfg.Provider,
		Families: append([]string{}, families...), Status: StatusRunning,
		StartedAt: b.cfg.Now().UTC()}
	b.record(run)
	return run, nil
}

func (b *LocalBench) execute(ctx context.Context, run LocalBenchRun) (LocalBenchRun,
	error) {
	c, err := b.provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return b.finish(run, StatusFailed, fmt.Errorf("create clone: %w", err))
	}
	run.CloneID = c.ID
	runErr := b.exercise(ctx, &run, c.DSN)
	status := StatusCompleted
	if runErr != nil {
		status = StatusFailed
	}
	if err := b.provider.Destroy(context.WithoutCancel(ctx), c); err != nil {
		status = StatusDestroyFailed
		runErr = errors.Join(runErr, fmt.Errorf("destroy clone %s: %w", c.ID, err))
	}
	return b.finish(run, status, runErr)
}

// exercise runs the fault programs on the clone and ingests the report
// as a local run of this pg_sage.
func (b *LocalBench) exercise(ctx context.Context, run *LocalBenchRun, dsn string) error {
	raw, err := b.faults.Run(ctx, dsn, run.Families)
	if err != nil {
		return fmt.Errorf("fault programs: %w", err)
	}
	stored, err := b.ledger.IngestBench(ctx, raw, earned.BenchIngest{
		Origin: earned.OriginLocalRun, Actor: earned.ActorPgSage})
	if err != nil {
		return fmt.Errorf("ingest local bench report: %w", err)
	}
	run.EvalRunID = stored.ID
	return nil
}

func (b *LocalBench) finish(run LocalBenchRun, status string, runErr error) (LocalBenchRun,
	error) {
	at := b.cfg.Now().UTC()
	run.Status, run.FinishedAt = status, &at
	if runErr != nil {
		run.Error = truncate(runErr.Error(), 2000)
	}
	b.record(run)
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
