package srebench

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// sampleInterval spaces the two samples connection and WAL plans take;
// the fault program's Between runs at its start.
const sampleInterval = 3 * time.Second

// Run runs every scenario in order: inject the fault, check that it
// manifests, investigate it through the real coordinator, then end its
// sessions, recover and check that the fault is gone (the post-fix
// verifier).
func Run(ctx context.Context, e *Env, ss []Scenario) []Result {
	out := make([]Result, 0, len(ss))
	for _, sc := range ss {
		out = append(out, e.runOne(ctx, sc))
	}
	return out
}

func (e *Env) runOne(ctx context.Context, sc Scenario) (r Result) {
	r.Scenario = sc
	defer func() {
		e.closeSessions()
		err := sc.Program.Recover(ctx, e)
		e.unlockCluster()
		if err != nil && r.Err == nil && r.Skipped == "" {
			r.Err = fmt.Errorf("recover: %w", err)
		}
	}()
	if err := sc.Program.Inject(ctx, e); err != nil {
		var u *Unsupported
		if errors.As(err, &u) {
			r.Skipped = u.Reason
			return r
		}
		r.Err = fmt.Errorf("inject: %w", err)
		return r
	}
	if err := sc.Program.Manifest(ctx, e); err != nil {
		r.Err = fmt.Errorf("fault did not manifest: %w", err)
		return r
	}
	r.Outcome, r.Err = e.investigate(ctx, sc)
	return r
}

// investigate runs one investigation of the scenario's family through a
// fresh coordinator (its own database identity), with the fault
// program's Between action at the start of the sample interval.
func (e *Env) investigate(ctx context.Context, sc Scenario) (Outcome, error) {
	cfg := sre.DefaultCoordinatorConfig(fmt.Sprintf("bench:%s:%d", sc.ID,
		time.Now().UnixNano()))
	cfg.SampleInterval = sampleInterval
	wait := func(ctx context.Context, d time.Duration) error {
		start := time.Now()
		if err := sc.Program.Between(ctx, e); err != nil {
			return fmt.Errorf("between samples: %w", err)
		}
		return sleepRest(ctx, d-time.Since(start))
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: e.Store, Runner: e.Runner,
		Config: cfg, Wait: wait})
	if err != nil {
		return Outcome{}, err
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		return Outcome{}, err
	}
	subject := sc.Subject
	if subject == "" {
		subject = "bench " + sc.ID
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "bench:" + sc.ID,
		Kind: sc.Family, Subject: subject})
	if err != nil {
		return Outcome{}, err
	}
	if err := coord.Investigate(ctx, inv.ID); err != nil {
		return Outcome{}, err
	}
	return e.outcome(ctx, scope, inv.ID)
}

func sleepRest(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// outcome reads the persisted diagnosis.
func (e *Env) outcome(ctx context.Context, scope sre.Scope, id sre.UUID) (Outcome, error) {
	inv, err := e.Store.Get(ctx, scope, id)
	if err != nil {
		return Outcome{}, err
	}
	if !inv.State.Terminal() || inv.State == sre.StateFailed {
		return Outcome{}, fmt.Errorf("investigation ended %s (%s)", inv.State,
			inv.FailureCode)
	}
	hs, err := e.Store.Hypotheses(ctx, scope, id)
	if err != nil {
		return Outcome{}, err
	}
	o := Outcome{State: inv.State, Root: inv.Summary.Root}
	for _, h := range hs {
		if h.Status == sre.HypothesisContributing {
			o.Contributing = append(o.Contributing, h.Node)
		}
	}
	return o, nil
}

// lockCluster serializes cluster-wide fault programs (WAL, slots, the
// archiver) with other test packages; the harness releases it after the
// scenario recovers.
func (e *Env) lockCluster(ctx context.Context) error {
	release, err := lockCluster(ctx, e.DSN)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.release = release
	e.mu.Unlock()
	return nil
}

func (e *Env) unlockCluster() {
	e.mu.Lock()
	release := e.release
	e.release = nil
	e.mu.Unlock()
	if release != nil {
		release()
	}
}
