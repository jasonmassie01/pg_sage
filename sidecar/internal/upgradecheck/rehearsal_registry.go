package upgradecheck

import (
	"context"
	"sync"
)

// RehearsalRegistry runs at most one upgrade rehearsal per database: a
// second request while one runs gets the running one.
type RehearsalRegistry struct {
	mu      sync.Mutex
	running map[string]*RunningRehearsal
}

// NewRehearsalRegistry returns an empty registry.
func NewRehearsalRegistry() *RehearsalRegistry {
	return &RehearsalRegistry{running: map[string]*RunningRehearsal{}}
}

// RunningRehearsal is a rehearsal the registry runs.
type RunningRehearsal struct {
	mu     sync.Mutex
	report RehearsalReport
	err    error
	done   chan struct{}
}

// Snapshot is the report so far (the final one once Done is closed).
func (h *RunningRehearsal) Snapshot() RehearsalReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.report
}

// Done is closed when the rehearsal has finished.
func (h *RunningRehearsal) Done() <-chan struct{} { return h.done }

// Result is the final report and error; call it after Done.
func (h *RunningRehearsal) Result() (RehearsalReport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.report, h.err
}

func (h *RunningRehearsal) set(r RehearsalReport) {
	h.mu.Lock()
	h.report = r
	h.mu.Unlock()
}

// Start runs opts in the background under ctx, unless a rehearsal of
// opts.Database is running: then it returns that one and false. Invalid
// options start nothing. done, when set, sees the result before the
// database is free for the next request. The caller detaches ctx from a
// request's lifetime (context.WithoutCancel) and bounds it.
func (g *RehearsalRegistry) Start(ctx context.Context, opts RehearsalOptions,
	done func(RehearsalReport, error)) (*RunningRehearsal, bool, error) {
	if err := opts.validate(); err != nil {
		return nil, false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if h, ok := g.running[opts.Database]; ok {
		return h, false, nil
	}
	h := &RunningRehearsal{done: make(chan struct{}),
		report: RehearsalReport{Database: opts.Database, State: RehearsalRequested,
			SourceMajor: opts.SourceMajor, TargetMajor: opts.TargetMajor,
			Provider: opts.ProviderName}}
	g.running[opts.Database] = h
	progress := opts.OnStep
	opts.OnStep = func(r RehearsalReport) {
		h.set(r)
		if progress != nil {
			progress(r)
		}
	}
	go g.run(ctx, opts, h, done)
	return h, true, nil
}

func (g *RehearsalRegistry) run(ctx context.Context, opts RehearsalOptions,
	h *RunningRehearsal, done func(RehearsalReport, error)) {
	rep, err := Rehearse(ctx, opts)
	h.mu.Lock()
	h.report, h.err = rep, err
	h.mu.Unlock()
	if done != nil {
		done(rep, err)
	}
	g.mu.Lock()
	delete(g.running, opts.Database)
	g.mu.Unlock()
	close(h.done)
}

// Running is the rehearsal of database in progress, if any.
func (g *RehearsalRegistry) Running(database string) (*RunningRehearsal, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.running[database]
	return h, ok
}
