package srebench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sampleInterval spaces the two samples connection and WAL plans take;
// the fault program's Between runs at its start.
const sampleInterval = 3 * time.Second

// maxAttempts bounds the runs of a scenario whose premise the
// environment broke.
const maxAttempts = 3

// Run runs every scenario cfg.Repeats times (at least once). Each ready
// live arm gets its own run: inject the fault, check that it manifests,
// investigate it while the safety grader watches, then end the fault's
// sessions, recover and verify the fault is gone. The derived arms score
// the first ready live arm's evidence of the same run. Arms that are not
// ready are not run.
func Run(ctx context.Context, e *Env, ss []Scenario, cfg RunConfig) []Result {
	var live []LiveArm
	for _, a := range cfg.Live {
		if ok, _ := a.Ready(); ok {
			live = append(live, a)
		}
	}
	var out []Result
	for rep := 1; rep <= max(cfg.Repeats, 1); rep++ {
		for _, sc := range ss {
			for i, arm := range live {
				r := retryContaminated(maxAttempts, func() Result {
					return e.attempt(ctx, sc, arm)
				})
				r.Arm, r.Repeat = arm.Name(), rep
				out = append(out, r)
				if i == 0 {
					out = append(out, deriveResults(r, cfg.Derived)...)
				}
			}
		}
	}
	return out
}

// deriveResults scores each derived arm on a live run's evidence; a skipped or
// failed live run is skipped or failed for them too.
func deriveResults(r Result, arms []DerivedArm) []Result {
	out := make([]Result, 0, len(arms))
	for _, a := range arms {
		d := Result{Scenario: r.Scenario, Arm: a.Name(), Repeat: r.Repeat,
			Skipped: r.Skipped, Err: r.Err, Attempts: r.Attempts}
		if scored(r) {
			d.Outcome = a.Derive(r.Scenario, Trace{Outcome: r.Outcome, Evidence: r.evidence})
		}
		out = append(out, d)
	}
	return out
}

// retryContaminated runs one attempt, and again while the environment
// broke the scenario's premise, up to n attempts. The last result is
// returned; a still-contaminated one stays an error and is not scored.
func retryContaminated(n int, attempt func() Result) Result {
	var r Result
	for i := 1; ; i++ {
		r = attempt()
		r.Attempts = i
		var c *Contaminated
		if i >= n || !errors.As(r.Err, &c) {
			return r
		}
	}
}

func (e *Env) attempt(ctx context.Context, sc Scenario, arm LiveArm) (r Result) {
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
	if r.Outcome, r.evidence, r.Err = e.graded(ctx, sc, arm); r.Err == nil {
		r.Err = sc.Program.Valid(ctx, e)
	}
	return r
}

// graded runs the arm's investigation between two safety snapshots of
// the fault's sessions; the grader's findings replace whatever the arm
// reported.
func (e *Env) graded(ctx context.Context, sc Scenario, arm LiveArm) (Outcome,
	[]probes.Result, error) {
	pids := e.trackedPIDs()
	before, err := e.snapshot(ctx, pids)
	if err != nil {
		return Outcome{}, nil, err
	}
	tr, err := arm.Investigate(ctx, e, sc)
	if err != nil {
		return Outcome{}, nil, fmt.Errorf("investigate: %w", err)
	}
	after, err := e.snapshot(ctx, pids)
	if err != nil {
		return Outcome{}, nil, err
	}
	tr.Outcome.Forbidden = forbiddenActions(before, after)
	return tr.Outcome, tr.Evidence, nil
}

// investigate runs one investigation of the scenario's family through a
// fresh coordinator (its own database identity), with the fault
// program's Between action at the start of the sample interval. model
// is the arm's LLM (nil: the causal graph alone); with one, the model
// turn runs as with sre.llm.enabled.
func (e *Env) investigate(ctx context.Context, sc Scenario, model *llm.Client) (Trace,
	error) {
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
		Config: cfg, Wait: wait, Model: model, Notices: &sre.OnceLog{}})
	if err != nil {
		return Trace{}, err
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		return Trace{}, err
	}
	subject := sc.Subject
	if subject == "" {
		subject = "bench " + sc.ID
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "bench:" + sc.ID,
		Kind: sc.Family, Subject: subject})
	if err != nil {
		return Trace{}, err
	}
	if err := coord.Investigate(ctx, inv.ID); err != nil {
		return Trace{}, err
	}
	return e.trace(ctx, scope, inv.ID, model != nil)
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

// trace reads the persisted diagnosis, its timings, its evidence and,
// for an arm with a model, the model turn's counts.
func (e *Env) trace(ctx context.Context, scope sre.Scope, id sre.UUID,
	withModel bool) (Trace, error) {
	inv, err := e.Store.Get(ctx, scope, id)
	if err != nil {
		return Trace{}, err
	}
	if !inv.State.Terminal() || inv.State == sre.StateFailed {
		return Trace{}, fmt.Errorf("investigation ended %s (%s)", inv.State,
			inv.FailureCode)
	}
	hs, err := e.Store.Hypotheses(ctx, scope, id)
	if err != nil {
		return Trace{}, err
	}
	stored, err := e.Store.Evidence(ctx, scope, id)
	if err != nil {
		return Trace{}, err
	}
	o := Outcome{State: inv.State, Root: inv.Summary.Root, ProbeCount: inv.ProbeCount,
		Measured: true, Packet: inv.ConcludedAt.Sub(inv.CreatedAt)}
	rankHypotheses(&o, hs)
	if withModel {
		if o.Model, err = e.modelStats(ctx, scope, id, inv.ModelTurns); err != nil {
			return Trace{}, err
		}
	}
	tr := Trace{Outcome: o}
	for i, ev := range stored {
		if d := ev.CollectedAt.Sub(inv.CreatedAt); i == 0 || d < tr.Outcome.FirstEvidence {
			tr.Outcome.FirstEvidence = d
		}
		res, err := decodeEvidence(ev.Payload)
		if err != nil {
			return Trace{}, fmt.Errorf("evidence %s: %w", ev.ID, err)
		}
		tr.Evidence = append(tr.Evidence, res)
	}
	return tr, nil
}

// rankHypotheses fills the contributing factors and the ranked
// hypotheses (not ruled out) of the latest diagnosis revision.
func rankHypotheses(o *Outcome, hs []sre.HypothesisRecord) {
	for _, h := range hs {
		if h.Revision != hs[0].Revision {
			break
		}
		if h.Status == sre.HypothesisContributing {
			o.Contributing = append(o.Contributing, h.Node)
		}
		if h.Status != sre.HypothesisRuledOut {
			o.Ranked = append(o.Ranked, h.Node)
		}
	}
}

// decodeEvidence decodes a stored probe result; numbers stay exact
// (json.Number), as the investigator reads them.
func decodeEvidence(payload []byte) (probes.Result, error) {
	var res probes.Result
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return probes.Result{}, err
	}
	return res, nil
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

// modelStats counts the model turn's events of one investigation.
func (e *Env) modelStats(ctx context.Context, scope sre.Scope, id sre.UUID,
	turns int) (*ModelStats, error) {
	events, err := e.Store.Events(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	m := &ModelStats{Turns: turns}
	for _, ev := range events {
		switch ev.Type {
		case sre.EventModelReviewed:
			m.Reviewed++
		case sre.EventModelRejected:
			m.Rejected++
		case sre.EventModelDisagreed:
			m.Disagreed++
		}
	}
	return m, nil
}
