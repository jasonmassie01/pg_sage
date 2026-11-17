package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/config"
)

// briefingOwner is the "briefing" reconfiguration owner (llm-06). It tracks
// every database runtime's briefing worker, as llmClients tracks LLM
// clients, and swaps their schedule, channels and webhook on a live change.
type briefingOwner struct {
	mu      sync.Mutex
	set     map[*briefing.Worker]struct{}
	current *config.BriefingConfig
}

// briefingWorkers is the process's briefing owner.
var briefingWorkers = newBriefingOwner()

func newBriefingOwner() *briefingOwner {
	return &briefingOwner{set: map[*briefing.Worker]struct{}{}}
}

func (o *briefingOwner) Name() string { return config.BriefingOwner }

// Prepare refuses an invalid schedule, so the change is rejected at apply
// time with the field named and no worker changes.
func (o *briefingOwner) Prepare(
	ctx context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	if desired.Config == nil {
		return nil, fmt.Errorf("briefing: desired config is nil")
	}
	next := desired.Config.Briefing
	if err := briefing.ValidateSchedule(next.Schedule); err != nil {
		return nil, fmt.Errorf("briefing.schedule %q: %w", next.Schedule, err)
	}
	prevCfg := desired.Config
	if active.Config != nil {
		prevCfg = active.Config
	}
	// ux-02b: briefing.slack_webhook_url and briefing.channels are the
	// managed Notifications channel; a reconcile that fails refuses the
	// change and leaves the tables as they were.
	if err := alertingRuntime.reconcileChange(ctx, prevCfg, desired.Config); err != nil {
		return nil, fmt.Errorf("briefing: %w", err)
	}
	return preparedBriefing{owner: o, prev: prevCfg.Briefing, next: next,
		prevCfg: prevCfg, nextCfg: desired.Config}, nil
}

// add tracks w and gives it the settings of the last live change, so a
// database added after the change follows the active config.
func (o *briefingOwner) add(w *briefing.Worker) {
	if w == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.set[w] = struct{}{}
	if o.current != nil {
		if err := w.Reconfigure(*o.current); err != nil {
			logWarn("briefing", "apply live settings to a new worker: %v", err)
		}
	}
}

func (o *briefingOwner) remove(w *briefing.Worker) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.set, w)
}

// track adds w until ctx ends; the returned channel closes once w is gone.
func (o *briefingOwner) track(ctx context.Context, w *briefing.Worker) <-chan struct{} {
	o.add(w)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		o.remove(w)
	}()
	return done
}

// workers is a snapshot of the tracked workers; RunRollup orders them.
func (o *briefingOwner) workers() []*briefing.Worker {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]*briefing.Worker, 0, len(o.set))
	for w := range o.set {
		out = append(out, w)
	}
	return out
}

// apply swaps every worker's settings; bc was validated by Prepare.
func (o *briefingOwner) apply(bc config.BriefingConfig) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for w := range o.set {
		if err := w.Reconfigure(bc); err != nil {
			return fmt.Errorf("briefing.schedule %q: %w", bc.Schedule, err)
		}
	}
	o.current = &bc
	return nil
}

type preparedBriefing struct {
	owner            *briefingOwner
	prev, next       config.BriefingConfig
	prevCfg, nextCfg *config.Config
}

func (p preparedBriefing) Commit(context.Context) error { return p.owner.apply(p.next) }

// Rollback restores the settings and reconciles the previous briefing
// channel back.
func (p preparedBriefing) Rollback(ctx context.Context) error {
	if err := p.owner.apply(p.prev); err != nil {
		return err
	}
	if p.prevCfg == nil || p.nextCfg == nil {
		return nil
	}
	return alertingRuntime.reconcileChange(ctx, p.nextCfg, p.prevCfg)
}

func (preparedBriefing) Drain(context.Context) error { return nil }

// registerBriefingOwner registers the briefing owner in every mode.
func registerBriefingOwner() {
	if configController == nil {
		return
	}
	if err := configController.RegisterOwner(briefingWorkers); err != nil {
		logWarn("config", "briefing reconfiguration owner: %v", err)
	}
}

// briefingRollup reports whether one process-level message covers every
// fleet database (briefing.fleet_mode rollup, the fleet default).
func briefingRollup(c *config.Config) bool { return c.RollsUpBriefings() }

// cycleBriefing is the worker a database's orchestrator cycle runs: none in
// roll-up mode, where the process-level roll-up runs every worker.
func cycleBriefing(c *config.Config, w *briefing.Worker) *briefing.Worker {
	if briefingRollup(c) {
		return nil
	}
	return w
}

// briefingRollupLoop is the process's one fleet roll-up loop. A briefing
// worker's schedule state (its last run) has one owner, so a new start
// stops the running loop and waits for it to exit before it starts another.
var briefingRollupLoop struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// startBriefingRollup runs the fleet roll-up on the orchestrator cadence
// until shutdown, when the mode rolls briefings up. It replaces a roll-up
// loop already running (a meta or fleet init run again in one process).
func startBriefingRollup() {
	if !briefingRollup(cfg) {
		return
	}
	parent := shutdownCtx
	if parent == nil {
		parent = context.Background()
	}
	interval := orchestratorInterval(cfg)
	briefingRollupLoop.mu.Lock()
	defer briefingRollupLoop.mu.Unlock()
	stopBriefingRollupLocked()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	briefingRollupLoop.cancel, briefingRollupLoop.done = cancel, done
	go runBriefingRollup(ctx, interval, done)
	logInfo("briefing", "fleet roll-up on: one briefing message covers every database")
}

// runBriefingRollup is the roll-up loop; it closes done when it returns.
func runBriefingRollup(ctx context.Context, interval time.Duration, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			briefing.RunRollup(ctx, briefingWorkers.workers(), time.Now())
		}
	}
}

// stopBriefingRollup stops the roll-up loop, if one runs, and waits for it.
func stopBriefingRollup() {
	briefingRollupLoop.mu.Lock()
	defer briefingRollupLoop.mu.Unlock()
	stopBriefingRollupLocked()
}

func stopBriefingRollupLocked() {
	if briefingRollupLoop.cancel == nil {
		return
	}
	briefingRollupLoop.cancel()
	<-briefingRollupLoop.done
	briefingRollupLoop.cancel, briefingRollupLoop.done = nil, nil
}
