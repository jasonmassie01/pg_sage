package sre

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-06: the model's follow-up probe is an investigation probe too; a
// probe refused its slot is run once more when the lease leaves time, so
// the model never reasons over a gap the investigation inflicted on
// itself. No concurrency test here: runProbe runs one probe in order; the
// limiter's concurrent counting is pinned in probes.

// probeStepStore is the store surface runProbe uses; any other method
// panics on the nil embedded interface.
type probeStepStore struct {
	CoordinatorStore
	commits []StepResult
}

func (s *probeStepStore) Limits() Limits { return DefaultLimits() }

func (s *probeStepStore) Get(context.Context, Scope, UUID) (Investigation, error) {
	return Investigation{}, nil
}

func (s *probeStepStore) Heartbeat(_ context.Context, l Lease) (Lease, error) {
	return l, nil
}

func (s *probeStepStore) CommitStep(_ context.Context, _ Lease,
	st StepResult) (Investigation, error) {
	s.commits = append(s.commits, st)
	return Investigation{}, nil
}

func modelProbeSession(r ProbeRunner, left time.Duration) (*modelSession, *probeStepStore) {
	st := &probeStepStore{}
	return &modelSession{c: &Coordinator{store: st, runner: r}, lease: leaseLeaving(left)}, st
}

func TestRunProbe_QueueRefusalIsRetriedAndCommittedOnce(t *testing.T) {
	r := newScriptedRunner().script(probes.LongTransactions,
		queueRefusal(probes.LongTransactions),
		rows(probes.LongTransactions, probes.Row{"pid": int64(4242)}))
	s, st := modelProbeSession(r, time.Hour)
	probe, key, rej, err := s.runProbe(context.Background(),
		ProposedProbe{ID: probes.LongTransactions})
	if err != nil || rej != nil || probe == nil || key == "" {
		t.Fatalf("runProbe = %+v %q %+v %v, want a committed probe", probe, key, rej, err)
	}
	if r.calls[probes.LongTransactions] != 2 {
		t.Fatalf("runs = %d, want 2 (one retry)", r.calls[probes.LongTransactions])
	}
	if len(st.commits) != 3 || len(st.commits[0].Results) != 1 ||
		!st.commits[0].Results[0].Status.Usable() ||
		st.commits[0].NextState != StateNeedsEvidence ||
		st.commits[1].NextState != StateCollecting ||
		st.commits[2].NextState != StateEvaluating {
		t.Fatalf("commits = %+v, want the usable result once, then collecting and "+
			"evaluating", st.commits)
	}
}

func TestRunProbe_TwoRefusalsCommitConcurrencyLimit(t *testing.T) {
	r := newScriptedRunner().script(probes.LongTransactions,
		queueRefusal(probes.LongTransactions))
	s, st := modelProbeSession(r, time.Hour)
	if _, _, rej, err := s.runProbe(context.Background(),
		ProposedProbe{ID: probes.LongTransactions}); err != nil || rej != nil {
		t.Fatalf("runProbe: rejection %+v, err %v", rej, err)
	}
	if r.calls[probes.LongTransactions] != 2 || len(st.commits) == 0 ||
		!probes.QueueRefused(st.commits[0].Results[0]) {
		t.Fatalf("runs %d commits %+v, want 2 runs and the refusal committed",
			r.calls[probes.LongTransactions], st.commits)
	}
}

// QueueWait+stepMargin equals the 2*stepMargin read gate, so a lease too
// short for a retry is too short for the first read: nothing runs.
func TestRunProbe_LeaseTooShortForARetryRunsNothing(t *testing.T) {
	r := newScriptedRunner().script(probes.LongTransactions,
		queueRefusal(probes.LongTransactions), rows(probes.LongTransactions))
	s, st := modelProbeSession(r, probes.QueueWait+stepMargin-200*time.Millisecond)
	_, _, rej, err := s.runProbe(context.Background(),
		ProposedProbe{ID: probes.LongTransactions})
	if err != nil || rej == nil || rej.Reason != RejectNoTime {
		t.Fatalf("runProbe: rejection %+v, err %v, want %s", rej, err, RejectNoTime)
	}
	if r.total() != 0 || len(st.commits) != 0 {
		t.Fatalf("runs %d commits %d, want none", r.total(), len(st.commits))
	}
}
