package pressure

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// guard-01b: an approved enforcement item runs only through the runner,
// which rechecks the job, the pressure and the backend before handing one
// cancel to the executor.

type fakeCanceller struct {
	mu    sync.Mutex
	calls []executor.BackendCancel
	err   error
}

func (f *fakeCanceller) CancelBackend(_ context.Context,
	req executor.BackendCancel) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.err != nil {
		return 0, f.err
	}
	return 9001, nil
}

type runnerRig struct {
	jobs     *fakeJobs
	verdicts *fakeVerdicts
	backends *fakeBackends
	exec     *fakeCanceller
	r        *Runner
	now      time.Time
}

func newRunnerRig(t *testing.T) *runnerRig {
	t.Helper()
	b := writingBackend(4242)
	rig := &runnerRig{jobs: &fakeJobs{jobs: []Job{enforceJob(true)}},
		verdicts: &fakeVerdicts{at: fixtureStart}, backends: &fakeBackends{recheck: &b},
		exec: &fakeCanceller{}, now: fixtureStart.Add(10 * time.Minute)}
	r, err := NewRunner(RunnerOptions{Jobs: rig.jobs, Pressure: rig.verdicts,
		Backends: rig.backends, Exec: rig.exec, ProtectedRoles: []string{"replicator"},
		Now: func() time.Time { return rig.now }})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	rig.r = r
	return rig
}

func approvedItem() store.QueuedAction {
	return store.QueuedAction{ID: 55, FindingID: 66, Status: "approved",
		ActionType: "cancel_backend", ProposedSQL: "SELECT pg_cancel_backend(4242)",
		IdentityKey: JobIdentityKey(7, 4242, fixtureStart)}
}

func TestPressureJobRunner_OwnsOnlyItsItems(t *testing.T) {
	rig := newRunnerRig(t)
	if !rig.r.Owns(approvedItem()) {
		t.Fatal("runner does not own its own item")
	}
	other := approvedItem()
	other.IdentityKey = "sre_proposal:0190c8d2-7f00-7000-8000-000000000001"
	typed := approvedItem()
	typed.ActionType = "terminate_backend"
	broken := approvedItem()
	broken.IdentityKey = JobIdentityPrefix + "garbage"
	for name, a := range map[string]store.QueuedAction{"other key": other,
		"terminate": typed, "malformed": broken} {
		if rig.r.Owns(a) {
			t.Errorf("runner owns %s item %+v", name, a)
		}
	}
}

// Happy path: a still-stopped job's still-writing backend is cancelled
// with the fresh identity, the approver and the job on the evidence.
func TestPressureJobRunner_CancelsTheRecheckedBackend(t *testing.T) {
	rig := newRunnerRig(t)
	run, err := rig.r.RunApproved(context.Background(), approvedItem(), 3)
	if err != nil {
		t.Fatalf("RunApproved: %v", err)
	}
	if run.ActionLogID != 9001 || len(rig.exec.calls) != 1 {
		t.Fatalf("run = %+v with %d cancels, want action 9001 once", run, len(rig.exec.calls))
	}
	c := rig.exec.calls[0]
	if c.Target.PID != 4242 || !c.Target.BackendStart.Equal(fixtureStart) ||
		c.Target.User != "app" || c.FindingID != 66 || c.ApprovedBy != 3 ||
		!c.ObservedAt.Equal(fixtureStart.Add(2*time.Minute)) {
		t.Fatalf("cancel request = %+v", c)
	}
	if len(c.ProtectedRoles) != 1 || c.Evidence["origin"] != "pressure_job" ||
		c.Evidence["job_id"] != int64(7) || c.Evidence["application_name"] != "backfill_orders" ||
		c.Evidence["queue_id"] != 55 {
		t.Fatalf("cancel evidence = %+v, protected %v", c.Evidence, c.ProtectedRoles)
	}
	if got := rig.verdicts.calls[0]; got.Table != "public.orders" || got.Job != "backfill_orders" {
		t.Fatalf("approval-time check = %+v", got)
	}
}

// Fail closed: every changed precondition refuses without a cancel.
func TestPressureJobRunner_RefusesWhenAPreconditionChanged(t *testing.T) {
	cases := map[string]struct {
		mutate func(*runnerRig, *store.QueuedAction)
		want   error
	}{
		"not approved": {func(_ *runnerRig, a *store.QueuedAction) { a.Status = "pending" },
			ErrJobState},
		"no approver": {nil, executor.ErrBackendApprovalRequired},
		"job gone": {func(r *runnerRig, _ *store.QueuedAction) { r.jobs.jobs = nil },
			ErrJobNotFound},
		"job expired": {func(r *runnerRig, _ *store.QueuedAction) {
			r.now = fixtureStart.Add(2 * time.Hour)
		}, ErrJobExpired},
		"pressure eased": {func(r *runnerRig, _ *store.QueuedAction) {
			r.verdicts.next = []Kind{Slow}
		}, ErrPressureEased},
		"pressure unreadable": {func(r *runnerRig, _ *store.QueuedAction) {
			r.verdicts.err = errors.New("statement timeout")
		}, ErrPressureEased},
		"backend gone": {func(r *runnerRig, _ *store.QueuedAction) {
			r.backends.recheck = nil
		}, executor.ErrBackendEvidenceStale},
		"backend probe failed": {func(r *runnerRig, _ *store.QueuedAction) {
			r.backends.err = errors.New("connection refused")
		}, nil},
		"malformed key": {func(_ *runnerRig, a *store.QueuedAction) {
			a.IdentityKey = JobIdentityPrefix + "x"
		}, ErrJobNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newRunnerRig(t)
			a, approver := approvedItem(), 3
			if tc.mutate == nil {
				approver = 0
			} else {
				tc.mutate(rig, &a)
			}
			_, err := rig.r.RunApproved(context.Background(), a, approver)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("RunApproved = %v, want %v", err, tc.want)
			}
			if len(rig.exec.calls) != 0 {
				t.Fatalf("a refused run still cancelled: %+v", rig.exec.calls)
			}
		})
	}
}

// Error propagation: the executor's refusal reaches the caller with the pid.
func TestPressureJobRunner_ExecutorErrorPropagates(t *testing.T) {
	rig := newRunnerRig(t)
	rig.exec.err = &executor.WithheldError{Decision: executor.ActionPolicyDecision{
		Decision: "blocked", BlockedReason: "emergency_stop"}}
	_, err := rig.r.RunApproved(context.Background(), approvedItem(), 3)
	var withheld *executor.WithheldError
	if !errors.As(err, &withheld) || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("RunApproved = %v, want the withheld decision naming pid 4242", err)
	}
}

func TestPressureJobRunner_NewRefusesMissingParts(t *testing.T) {
	full := RunnerOptions{Jobs: &fakeJobs{}, Pressure: &fakeVerdicts{},
		Backends: &fakeBackends{}, Exec: &fakeCanceller{}}
	for name, mutate := range map[string]func(*RunnerOptions){
		"no jobs":     func(o *RunnerOptions) { o.Jobs = nil },
		"no pressure": func(o *RunnerOptions) { o.Pressure = nil },
		"no backends": func(o *RunnerOptions) { o.Backends = nil },
		"no executor": func(o *RunnerOptions) { o.Exec = nil },
		"evidence age over the ceiling": func(o *RunnerOptions) {
			o.MaxEvidenceAge = executor.MaxBackendEvidenceAge + time.Second
		},
	} {
		o := full
		mutate(&o)
		if _, err := NewRunner(o); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: NewRunner = %v, want ErrInvalidRequest", name, err)
		}
	}
}
