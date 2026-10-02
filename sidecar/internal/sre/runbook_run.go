package sre

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/runbook"
)

// Running a signed runbook inside an investigation (AI-SRE-SPEC §7.1).
// After the plan's deterministic diagnosis, the most specific signed
// runbook matching the trigger kind (and, when it names graph nodes, an
// open hypothesis) walks its DAG: probe steps extend the evidence within
// the probe ceiling and the active time, decisions are evaluated on the
// stored evidence and the re-run diagnosis, and the run ends in a proposal
// (never executed), an abstention or a budget stop. One runbook runs per
// investigation; the run is recorded with the version and content hash.

// RunbookSource is the store surface that serves signed runbooks; a store
// without it runs none.
type RunbookSource interface {
	RunnableRunbooks(ctx context.Context, scope Scope) ([]Runbook, error)
	RecordRunbookRun(ctx context.Context, lease Lease, run RunbookRun) error
}

var _ RunbookSource = (*PostgresStore)(nil)

// applyRunbook runs the matching signed runbook, if any, and returns the
// current lease, diagnosis and evidence and the recorded run. A failed
// runbook lookup never blocks the investigation.
func (c *Coordinator) applyRunbook(ctx context.Context, lease Lease, inv Investigation,
	d causal.Diagnosis, ev []Evidence) (Lease, causal.Diagnosis, []Evidence,
	*RunbookRun, error) {
	src, ok := c.store.(RunbookSource)
	if !ok {
		return lease, d, ev, nil, nil
	}
	rbs, err := src.RunnableRunbooks(ctx, lease.Scope)
	if err != nil {
		if ctx.Err() != nil {
			return lease, d, ev, nil, ctx.Err()
		}
		c.logFn("WARN", "sre: investigation %s: reading signed runbooks failed; "+
			"continuing without a runbook: %v", inv.ID, err)
		return lease, d, ev, nil, nil
	}
	rb, ok := pickRunbook(rbs, inv.TriggerKind, openNodes(d))
	if !ok {
		return lease, d, ev, nil, nil
	}
	w := &runbookWalk{c: c, lease: lease, inv: inv, rb: rb, d: d, ev: ev}
	run, err := w.run(ctx)
	if err != nil {
		return w.lease, w.d, w.ev, nil, err
	}
	if err := src.RecordRunbookRun(ctx, w.lease, run); err != nil {
		if errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrMetadataUnavailable) {
			return w.lease, w.d, w.ev, nil, err
		}
		c.logFn("WARN", "sre: investigation %s: recording runbook %s v%d failed: %v",
			inv.ID, rb.ID, rb.Latest.Version, err)
	}
	return w.lease, w.d, w.ev, &run, nil
}

// pickRunbook chooses the matching runbook naming the most open graph
// nodes; ties go to the name, then the id.
func pickRunbook(rbs []Runbook, kind TriggerKind, open []string) (Runbook, bool) {
	type candidate struct {
		rb       Runbook
		specific int
	}
	var cs []candidate
	for _, rb := range rbs {
		if ok, n := rb.Latest.Definition.Match(string(kind), open); ok && rb.Runnable {
			cs = append(cs, candidate{rb, n})
		}
	}
	if len(cs) == 0 {
		return Runbook{}, false
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.specific != b.specific {
			return a.specific > b.specific
		}
		if a.rb.Latest.Name != b.rb.Latest.Name {
			return a.rb.Latest.Name < b.rb.Latest.Name
		}
		return a.rb.ID < b.rb.ID
	})
	return cs[0].rb, true
}

// runbookWalk is one runbook run.
type runbookWalk struct {
	c          *Coordinator
	lease      Lease
	inv        Investigation
	rb         Runbook
	d          causal.Diagnosis
	ev         []Evidence
	collecting bool
}

func (w *runbookWalk) record() RunbookRun {
	v := w.rb.Latest
	return RunbookRun{Label: RunbookRunLabel, RunbookID: w.rb.ID, Version: v.Version,
		Name: v.Name, ContentHash: v.ContentHash, SignedBy: v.SignedBy, Path: []string{},
		CreatedAt: time.Now().UTC()}
}

// run walks the DAG from its start. The DAG is acyclic, so the walk ends
// within one visit per node.
func (w *runbookWalk) run(ctx context.Context) (RunbookRun, error) {
	def := w.rb.Latest.Definition
	out := w.record()
	id := def.Start
	for range def.Nodes {
		n, ok := def.Node(id)
		if !ok {
			break
		}
		out.Path = append(out.Path, n.ID)
		next, done, err := w.step(ctx, n, &out)
		if err != nil || done {
			return out, errors.Join(err, w.evaluate(ctx))
		}
		id = next
	}
	out.Outcome, out.Reason = RunbookAbstained, "the walk did not reach a proposal"
	return out, w.evaluate(ctx)
}

// step runs one node and returns the next node id, or done.
func (w *runbookWalk) step(ctx context.Context, n runbook.Node,
	out *RunbookRun) (string, bool, error) {
	switch n.Type {
	case runbook.NodeProbe:
		stop, err := w.probe(ctx, n, out)
		return n.Next, stop, err
	case runbook.NodeDecision:
		t, why := runbook.Eval(*n.When, w.env())
		switch t {
		case runbook.True:
			return n.Then, false, nil
		case runbook.False:
			return n.Else, false, nil
		}
		out.Outcome = RunbookAbstained
		out.Reason = truncateRunes(RedactText(n.ID+": "+why), maxRunReason)
		return "", true, nil
	}
	p := n.Proposal
	out.Outcome = RunbookCompleted
	out.Proposal = &RunbookProposal{Label: RunbookProposalLabel, Kind: string(p.Kind),
		Node: p.Node, ActionType: p.ActionType, Text: p.Text()}
	return "", true, nil
}

// probe runs one probe step under the probe ceiling and the active time.
// A step committed by an earlier (crashed) run of this investigation is
// not run again.
func (w *runbookWalk) probe(ctx context.Context, n runbook.Node,
	out *RunbookRun) (bool, error) {
	key := fmt.Sprintf("runbook-%s-v%d-%s", w.rb.ID, w.rb.Latest.Version, n.ID)
	for _, e := range w.ev {
		if e.StepKey == key {
			out.Probes++
			return false, nil
		}
	}
	cur, err := w.c.store.Get(ctx, w.lease.Scope, w.inv.ID)
	if err != nil {
		return true, err
	}
	if ceiling := w.c.store.Limits().MaxProbes; cur.ProbeCount+1 > ceiling {
		out.Outcome = RunbookProbeBudget
		out.Reason = fmt.Sprintf("%s: %d probes ran; the ceiling is %d", n.ID,
			cur.ProbeCount, ceiling)
		return true, nil
	}
	if time.Until(w.lease.SegmentDeadline) < 2*stepMargin {
		out.Outcome, out.Reason = RunbookNoTime, n.ID+": no active time left"
		return true, nil
	}
	if err := w.toCollecting(ctx, cur.State); err != nil {
		return true, err
	}
	if w.lease, err = w.c.store.Heartbeat(ctx, w.lease); err != nil {
		return true, err
	}
	res := w.c.runner.Run(ctx, probes.ID(n.Probe), stepArgs(n))
	_, err = w.c.store.CommitStep(ctx, w.lease, StepResult{IdempotencyKey: key,
		Results: []probes.Result{res}, NextState: StateCollecting})
	if errors.Is(err, ErrBudgetExhausted) {
		out.Outcome, out.Reason = RunbookProbeBudget, n.ID+": "+err.Error()
		return true, nil
	}
	if err != nil {
		return true, err
	}
	out.Probes++
	return false, w.refresh(ctx)
}

func stepArgs(n runbook.Node) probes.Args {
	if n.Args == nil {
		return probes.Args{}
	}
	return probes.Args{Window: time.Duration(n.Args.WindowSeconds) * time.Second}
}

// toCollecting moves the investigation back to collecting before the
// first runbook probe: evaluating -> needs_evidence -> collecting.
func (w *runbookWalk) toCollecting(ctx context.Context, state State) error {
	if w.collecting || state == StateCollecting {
		w.collecting = true
		return nil
	}
	path := []State{StateNeedsEvidence, StateCollecting}
	if state == StateNeedsEvidence {
		path = path[1:]
	}
	for _, next := range path {
		if _, err := w.c.store.CommitStep(ctx, w.lease, StepResult{NextState: next,
			IdempotencyKey: fmt.Sprintf("runbook-%s-f%d", next, w.lease.Fence)}); err != nil {
			return err
		}
	}
	w.collecting = true
	return nil
}

// evaluate returns the investigation to evaluating after runbook probes.
func (w *runbookWalk) evaluate(ctx context.Context) error {
	if !w.collecting {
		return nil
	}
	_, err := w.c.store.CommitStep(ctx, w.lease, StepResult{NextState: StateEvaluating,
		IdempotencyKey: fmt.Sprintf("runbook-evaluate-f%d", w.lease.Fence)})
	return err
}

// refresh re-reads the evidence and re-runs the diagnosis on all of it.
func (w *runbookWalk) refresh(ctx context.Context) error {
	ev, err := w.c.store.Evidence(ctx, w.lease.Scope, w.inv.ID)
	if err != nil {
		return err
	}
	obs, err := observations(ev)
	if err != nil {
		w.c.logFn("WARN", "sre: investigation %s: runbook evidence unreadable: %v",
			w.inv.ID, err)
		return nil
	}
	w.ev, w.d = ev, diagnose(w.inv, obs)
	return nil
}

// env is what decisions read: the latest stored result of each probe and
// the current diagnosis.
func (w *runbookWalk) env() runbook.Env {
	e := walkEnv{latest: map[probes.ID]probes.Result{},
		status: map[causal.NodeID]causal.Status{}}
	if obs, err := observations(w.ev); err == nil {
		for _, o := range obs {
			e.latest[o.Result.ProbeID] = o.Result
		}
	}
	add := func(hs []causal.Hypothesis, st causal.Status) {
		for _, h := range hs {
			e.status[h.Node] = st
		}
	}
	add(w.d.RuledOut, causal.StatusRuledOut)
	add(w.d.Alternatives, causal.StatusAlternative)
	add(w.d.Contributing, causal.StatusContributing)
	if w.d.Root != nil {
		e.status[w.d.Root.Node] = causal.StatusAlternative
		if w.d.Conclusive {
			e.status[w.d.Root.Node] = causal.StatusRoot
		}
	}
	return e
}

type walkEnv struct {
	latest map[probes.ID]probes.Result
	status map[causal.NodeID]causal.Status
}

func (e walkEnv) Latest(id probes.ID) (probes.Result, bool) {
	r, ok := e.latest[id]
	return r, ok
}

func (e walkEnv) Hypothesis(node causal.NodeID) (causal.Status, bool) {
	s, ok := e.status[node]
	return s, ok
}
