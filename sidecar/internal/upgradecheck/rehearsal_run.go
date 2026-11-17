package upgradecheck

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// rehearsalCleanupTimeout bounds the clone's teardown, which runs on a
// fresh context because the caller's may have ended.
const rehearsalCleanupTimeout = 10 * time.Minute

// Rehearse runs one upgrade rehearsal (see rehearsal.go). The error is
// why it failed, also recorded in the report; the clone, once created, is
// always destroyed, and a teardown that fails leaves its receipt to the
// reaper.
func Rehearse(ctx context.Context, opts RehearsalOptions) (RehearsalReport, error) {
	if err := opts.validate(); err != nil {
		return RehearsalReport{Database: opts.Database, State: RehearsalFailed,
			Error: err.Error()}, err
	}
	r := &rehearsalRun{opts: opts.withDefaults()}
	r.rep = RehearsalReport{Database: opts.Database, Provider: opts.ProviderName,
		SourceMajor: opts.SourceMajor, TargetMajor: opts.TargetMajor,
		StartedAt: time.Now().UTC()}
	r.step(RehearsalRequested, "")
	err := r.execute(ctx)
	r.rep.FinishedAt = time.Now().UTC()
	return r.snapshot(), err
}

type rehearsalRun struct {
	opts RehearsalOptions
	rep  RehearsalReport
}

func (r *rehearsalRun) step(state, detail string) {
	r.rep.State = state
	r.rep.Steps = append(r.rep.Steps, RehearsalStep{State: state, At: time.Now().UTC(),
		Detail: detail})
	if r.opts.OnStep != nil {
		r.opts.OnStep(r.snapshot())
	}
}

// snapshot is a copy of the report that shares no slice with the run.
func (r *rehearsalRun) snapshot() RehearsalReport {
	out := r.rep
	out.Steps = slices.Clone(r.rep.Steps)
	out.Changed = slices.Clone(r.rep.Changed)
	out.NotCompared = slices.Clone(r.rep.NotCompared)
	return out
}

func (r *rehearsalRun) fail(err error) error {
	r.rep.Error = err.Error()
	r.step(RehearsalFailed, err.Error())
	return err
}

func (r *rehearsalRun) execute(ctx context.Context) error {
	top, err := r.opts.Top(ctx, MaxRehearsalQueries)
	if err != nil {
		return r.fail(fmt.Errorf("read production's top statements: %w", err))
	}
	if len(top) > MaxRehearsalQueries {
		top = top[:MaxRehearsalQueries]
	}
	cl, err := r.opts.Provider.Create(ctx, clone.CloneSpec{IncludeData: true})
	if err != nil {
		return r.fail(fmt.Errorf("create the rehearsal clone: %w", err))
	}
	r.rep.CloneID = cl.ID
	if id, ok := r.opts.Provider.(clone.ReceiptNamer).ReceiptOf(cl.ID); ok {
		r.rep.ReceiptID = id
	}
	r.step(RehearsalCloned, cl.ID)
	err = r.onClone(ctx, cl, top)
	if err != nil {
		_ = r.fail(err)
	}
	if derr := r.destroy(ctx, cl); err == nil {
		err = derr
	}
	return err
}

func (r *rehearsalRun) destroy(ctx context.Context, cl clone.Clone) error {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rehearsalCleanupTimeout)
	defer cancel()
	if err := r.opts.Provider.Destroy(dctx, cl); err != nil {
		err = fmt.Errorf("destroy the rehearsal clone %s (its receipt lets the reaper "+
			"retry): %w", cl.ID, err)
		r.rep.CleanupError = err.Error()
		r.step(RehearsalFailed, err.Error())
		return err
	}
	r.step(RehearsalDestroyed, cl.ID)
	return nil
}

// onClone captures before the upgrade, upgrades the clone, captures as
// cut over, runs ANALYZE and captures again.
func (r *rehearsalRun) onClone(ctx context.Context, cl clone.Clone, top []TopQuery) error {
	queries := prepareQueries(top, r.opts.SourceMajor, r.opts.TargetMajor)
	if err := r.capture(ctx, cl, r.opts.SourceMajor, queries, before); err != nil {
		return err
	}
	up, err := r.upgrade(ctx, cl)
	if err != nil {
		return err
	}
	if err := r.capture(ctx, up, r.opts.TargetMajor, queries, asCutOver); err != nil {
		return err
	}
	if err := r.analyze(ctx, up); err != nil {
		return err
	}
	if err := r.capture(ctx, up, r.opts.TargetMajor, queries, afterAnalyze); err != nil {
		return err
	}
	for i := range queries {
		compareCaptures(&queries[i])
	}
	summarize(&r.rep, queries)
	return nil
}

func (r *rehearsalRun) upgrade(ctx context.Context, cl clone.Clone) (clone.Clone, error) {
	start := time.Now()
	r.rep.UpgradeAt = start.UTC()
	up, err := r.opts.Upgrader.Upgrade(ctx, cl, r.opts.TargetMajor)
	r.rep.UpgradeSeconds = time.Since(start).Seconds()
	if err != nil {
		r.rep.ProviderMessage = err.Error()
		return clone.Clone{}, fmt.Errorf("upgrade the rehearsal clone %s to PostgreSQL %d: %w",
			cl.ID, r.opts.TargetMajor, err)
	}
	if up.ID != cl.ID {
		return clone.Clone{}, fmt.Errorf("%w: the upgrader answered with clone %q, not the "+
			"rehearsal clone %q", ErrUpgradeRefused, up.ID, cl.ID)
	}
	r.step(RehearsalUpgraded, fmt.Sprintf("PostgreSQL %d in %.1fs", r.opts.TargetMajor,
		r.rep.UpgradeSeconds))
	return up, nil
}

// analyze runs ANALYZE on the upgraded clone (shown not to be
// production) under the analyze timeout.
func (r *rehearsalRun) analyze(ctx context.Context, cl clone.Clone) error {
	pool, err := clone.GuardedConnect(ctx, cl, r.opts.Production)
	if err != nil {
		return fmt.Errorf("connect the upgraded clone to ANALYZE: %w", err)
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect the upgraded clone to ANALYZE: %w", err)
	}
	defer conn.Release()
	ms := fmt.Sprintf("%dms", r.opts.AnalyzeTimeout.Milliseconds())
	if _, err := conn.Exec(ctx, "SELECT pg_catalog.set_config('statement_timeout', $1, "+
		"false)", ms); err != nil {
		return fmt.Errorf("bound ANALYZE on the clone: %w", err)
	}
	start := time.Now()
	r.rep.AnalyzeAt = start.UTC()
	_, err = conn.Exec(ctx, "ANALYZE")
	r.rep.AnalyzeSeconds = time.Since(start).Seconds()
	if err != nil {
		return fmt.Errorf("ANALYZE the upgraded clone %s (bound %s): %w", cl.ID,
			r.opts.AnalyzeTimeout, err)
	}
	r.step(RehearsalAnalyzed, fmt.Sprintf("in %.1fs", r.rep.AnalyzeSeconds))
	return nil
}
