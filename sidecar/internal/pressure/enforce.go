package pressure

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Approval-gated stop enforcement (guard-01b). For each active job
// registered with enforce, the enforcer checks pressure on the job's table
// once per tick. When the answer is stop for Stops consecutive read sets
// and the job's backend is still writing, it proposes cancel_backend for
// that backend in the approval queue, once per backend incarnation, with
// the verdict's inputs on the card. It never cancels: only a person's
// approval runs the cancel, through the executor (Runner).

const (
	// DefaultStops is how many consecutive stops propose a cancel.
	DefaultStops = 2
	// DefaultApprovalTTL is a proposal's life: a backend signal card
	// lives at most 15 minutes (approvalcard.BackendSignalCardTTL).
	DefaultApprovalTTL = 15 * time.Minute
	// DefaultMaxBackends bounds the proposals of one job per tick.
	DefaultMaxBackends = 4
)

// VerdictSource answers one check (the database's Checker).
type VerdictSource interface {
	Check(ctx context.Context, req Request) (Verdict, error)
}

// JobLister lists a database's active jobs.
type JobLister interface {
	Active(ctx context.Context, database string) ([]Job, error)
}

// Proposal is one cancel the enforcer asks a person to approve.
type Proposal struct {
	Job       Job
	Backend   Backend
	Verdict   Verdict
	Stops     int
	ExpiresAt time.Time
}

// Proposer queues a proposal for approval once per job and backend
// incarnation: a second call returns the same item with created false.
type Proposer interface {
	Propose(ctx context.Context, p Proposal) (queueID int, created bool, err error)
}

// EnforcerOptions configure an Enforcer; zero values are the defaults.
type EnforcerOptions struct {
	Database    string
	Jobs        JobLister
	Pressure    VerdictSource
	Backends    BackendFinder
	Proposals   Proposer
	Stops       int
	ApprovalTTL time.Duration
	MaxBackends int
	Now         func() time.Time
}

// Enforcer watches one database's enforced jobs.
type Enforcer struct {
	o  EnforcerOptions
	mu sync.Mutex
	// streaks are each job's consecutive stops and the read set last
	// counted (a cached answer seen again is not another stop).
	streaks map[int64]streak
	// proposed are the backend incarnations already queued, per job.
	proposed map[int64]map[string]bool
}

type streak struct {
	stops int
	last  time.Time
}

// TickResult is what one tick did.
type TickResult struct {
	Checked  int
	Proposed int
}

// NewEnforcer checks the options and fills the defaults.
func NewEnforcer(o EnforcerOptions) (*Enforcer, error) {
	switch {
	case o.Database == "":
		return nil, fmt.Errorf("%w: the enforcer needs its database", ErrInvalidRequest)
	case o.Jobs == nil || o.Pressure == nil || o.Backends == nil || o.Proposals == nil:
		return nil, fmt.Errorf("%w: the enforcer needs jobs, pressure, backends and an "+
			"approval queue", ErrInvalidRequest)
	case o.Stops != 0 && o.Stops < DefaultStops:
		return nil, fmt.Errorf("%w: %d consecutive stops is under the minimum %d",
			ErrInvalidRequest, o.Stops, DefaultStops)
	}
	if o.Stops == 0 {
		o.Stops = DefaultStops
	}
	if o.ApprovalTTL <= 0 || o.ApprovalTTL > DefaultApprovalTTL {
		o.ApprovalTTL = DefaultApprovalTTL
	}
	if o.MaxBackends <= 0 {
		o.MaxBackends = DefaultMaxBackends
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Enforcer{o: o, streaks: map[int64]streak{},
		proposed: map[int64]map[string]bool{}}, nil
}

// Tick checks every active enforced job once. A job's failure is joined
// into the error and the other jobs still run.
func (e *Enforcer) Tick(ctx context.Context) (TickResult, error) {
	jobs, err := e.o.Jobs.Active(ctx, e.o.Database)
	if err != nil {
		return TickResult{}, fmt.Errorf("pressure enforcement of %s: %w", e.o.Database, err)
	}
	var res TickResult
	var errs []error
	live := map[int64]bool{}
	for _, j := range jobs {
		if !j.Enforce {
			continue
		}
		live[j.ID] = true
		res.Checked++
		v, err := e.o.Pressure.Check(ctx, Request{Database: e.o.Database, Table: j.Table,
			Job: j.ApplicationName})
		if err != nil {
			errs = append(errs, fmt.Errorf("pressure check of job %q: %w",
				j.ApplicationName, err))
			continue
		}
		n, err := e.Observe(ctx, j, v)
		res.Proposed += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	e.forget(live)
	return res, errors.Join(errs...)
}

// forget drops what it kept for jobs that are no longer active.
func (e *Enforcer) forget(live map[int64]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id := range e.streaks {
		if !live[id] {
			delete(e.streaks, id)
		}
	}
	for id := range e.proposed {
		if !live[id] {
			delete(e.proposed, id)
		}
	}
}

// Observe counts one verdict for job and proposes a cancel for each of
// its writing backends once the stops reach the threshold. It returns the
// number of new approval items. Calls are serialized, so concurrent stops
// for one job propose once.
func (e *Enforcer) Observe(ctx context.Context, j Job, v Verdict) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.streaks[j.ID]
	if !v.CheckedAt.IsZero() && v.CheckedAt.Equal(s.last) {
		return 0, nil
	}
	s.last = v.CheckedAt
	if v.Verdict != Stop {
		s.stops = 0
	} else {
		s.stops++
	}
	e.streaks[j.ID] = s
	if s.stops < e.o.Stops {
		return 0, nil
	}
	return e.proposeFor(ctx, j, v, s.stops)
}

// proposeFor queues one cancel per writing backend of j not yet proposed.
func (e *Enforcer) proposeFor(ctx context.Context, j Job, v Verdict, stops int) (int, error) {
	backends, err := e.o.Backends.Writing(ctx, j.ApplicationName)
	if err != nil {
		return 0, fmt.Errorf("reading the writing sessions of job %q: %w",
			j.ApplicationName, err)
	}
	done := e.proposed[j.ID]
	if done == nil {
		done = map[string]bool{}
		e.proposed[j.ID] = done
	}
	created := 0
	for i, b := range backends {
		if i >= e.o.MaxBackends {
			break
		}
		key := JobIdentityKey(j.ID, b.PID, b.BackendStart)
		if done[key] {
			continue
		}
		_, isNew, err := e.o.Proposals.Propose(ctx, Proposal{Job: j, Backend: b, Verdict: v,
			Stops: stops, ExpiresAt: e.o.Now().Add(e.o.ApprovalTTL)})
		if err != nil {
			return created, fmt.Errorf("proposing the cancel of pid %d for job %q: %w",
				b.PID, j.ApplicationName, err)
		}
		done[key] = true
		if isNew {
			created++
		}
	}
	return created, nil
}
