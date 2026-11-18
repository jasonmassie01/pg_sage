package pressure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/store"
)

// Runner runs an approved enforcement item (executor.ApprovedActionRunner).
// It fails closed: the job must still be registered, the pressure must
// still read stop and the backend incarnation must still be writing under
// the job's name, or nothing is signalled. Then it hands exactly one
// cancel to the executor, which authorizes it at the standing policy gate
// as an operator-approved backend signal and rechecks the identity in the
// signalling statement.

var (
	// ErrJobState is an item that is not an approved enforcement item.
	ErrJobState = errors.New("pressure job item is not approved")
	// ErrPressureEased is an approval that came after the pressure eased
	// (or could no longer be read): the cancel is refused.
	ErrPressureEased = errors.New("pressure no longer reads stop")
)

// Canceller is the executor half of an approved cancel.
type Canceller interface {
	CancelBackend(ctx context.Context, req executor.BackendCancel) (int64, error)
}

// JobGetter reads one job.
type JobGetter interface {
	Get(ctx context.Context, id int64) (Job, error)
}

// RunnerOptions configure a Runner.
type RunnerOptions struct {
	Jobs     JobGetter
	Pressure VerdictSource
	Backends BackendFinder
	Exec     Canceller
	// ProtectedRoles and ProtectedApplications are never signalled.
	ProtectedRoles        []string
	ProtectedApplications []string
	// MaxEvidenceAge is in (0, executor.MaxBackendEvidenceAge]; 0 is the
	// ceiling.
	MaxEvidenceAge time.Duration
	Now            func() time.Time
}

// Runner runs approved enforcement items.
type Runner struct{ o RunnerOptions }

// NewRunner checks the options.
func NewRunner(o RunnerOptions) (*Runner, error) {
	switch {
	case o.Jobs == nil || o.Pressure == nil || o.Backends == nil || o.Exec == nil:
		return nil, fmt.Errorf("%w: the runner needs jobs, pressure, backends and the "+
			"executor", ErrInvalidRequest)
	case o.MaxEvidenceAge < 0 || o.MaxEvidenceAge > executor.MaxBackendEvidenceAge:
		return nil, fmt.Errorf("%w: evidence age %s outside (0, %s]", ErrInvalidRequest,
			o.MaxEvidenceAge, executor.MaxBackendEvidenceAge)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Runner{o: o}, nil
}

// Owns reports an enforcement item: its key parses and it cancels.
func (r *Runner) Owns(a store.QueuedAction) bool {
	_, _, _, ok := ParseJobIdentityKey(a.IdentityKey)
	return ok && a.ActionType == "cancel_backend"
}

// RunApproved rechecks the item's job, pressure and backend and runs one
// cancel for approvedBy.
func (r *Runner) RunApproved(ctx context.Context, a store.QueuedAction,
	approvedBy int) (executor.ApprovedRun, error) {
	if approvedBy <= 0 {
		return executor.ApprovedRun{}, executor.ErrBackendApprovalRequired
	}
	if a.Status != "approved" {
		return executor.ApprovedRun{}, fmt.Errorf("%w: queue item %d is %s", ErrJobState,
			a.ID, a.Status)
	}
	jobID, pid, start, ok := ParseJobIdentityKey(a.IdentityKey)
	if !ok {
		return executor.ApprovedRun{}, fmt.Errorf("%w: queue item %d key %q",
			ErrJobNotFound, a.ID, a.IdentityKey)
	}
	j, err := r.o.Jobs.Get(ctx, jobID)
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	if !j.Active(r.o.Now()) {
		return executor.ApprovedRun{}, fmt.Errorf("%w: job %q (%d) ended %s; nothing was "+
			"cancelled", ErrJobExpired, j.ApplicationName, j.ID, j.ExpiresAt.Format(time.RFC3339))
	}
	v, err := r.stillStopped(ctx, j)
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	b, err := r.recheck(ctx, j, pid, start)
	if err != nil {
		return executor.ApprovedRun{}, err
	}
	return r.cancel(ctx, a, j, b, v, approvedBy)
}

// stillStopped refuses unless the job's table reads stop now.
func (r *Runner) stillStopped(ctx context.Context, j Job) (Verdict, error) {
	v, err := r.o.Pressure.Check(ctx, Request{Database: j.Database, Table: j.Table,
		Job: j.ApplicationName})
	if err != nil {
		return v, fmt.Errorf("%w: the pressure of job %q could not be read (%v); nothing "+
			"was cancelled", ErrPressureEased, j.ApplicationName, err)
	}
	if v.Verdict != Stop {
		return v, fmt.Errorf("%w: job %q now reads %s; nothing was cancelled",
			ErrPressureEased, j.ApplicationName, v.Verdict)
	}
	return v, nil
}

// recheck reads the approved incarnation again; gone or idle is stale.
func (r *Runner) recheck(ctx context.Context, j Job, pid int32,
	start time.Time) (Backend, error) {
	b, ok, err := r.o.Backends.Recheck(ctx, j.ApplicationName, pid, start)
	if err != nil {
		return Backend{}, fmt.Errorf("rechecking pid %d of job %q: %w", pid,
			j.ApplicationName, err)
	}
	if !ok {
		return Backend{}, fmt.Errorf("%w: pid %d of job %q is gone or no longer writing; "+
			"nothing was cancelled", executor.ErrBackendEvidenceStale, pid, j.ApplicationName)
	}
	return b, nil
}

func (r *Runner) cancel(ctx context.Context, a store.QueuedAction, j Job, b Backend,
	v Verdict, approvedBy int) (executor.ApprovedRun, error) {
	id, err := r.o.Exec.CancelBackend(ctx, executor.BackendCancel{Target: b.Identity(),
		ObservedAt: b.ObservedAt, MaxEvidenceAge: r.o.MaxEvidenceAge, FindingID: a.FindingID,
		ApprovedBy: approvedBy, ProtectedRoles: r.o.ProtectedRoles,
		ProtectedApplications: r.o.ProtectedApplications,
		Evidence: map[string]any{"origin": FindingCategory, "job_id": j.ID,
			"application_name": j.ApplicationName, "queue_id": a.ID,
			"pressure_reasons": strings.Join(v.Reasons, "; ")}})
	if err != nil {
		return executor.ApprovedRun{}, fmt.Errorf("cancelling pid %d of job %q: %w", b.PID,
			j.ApplicationName, err)
	}
	return executor.ApprovedRun{ActionLogID: id}, nil
}
