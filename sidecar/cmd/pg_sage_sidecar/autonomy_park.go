package main

import (
	"errors"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/schemaguard"
)

// parkExpectedRefusal turns the executor's expected refusals of a
// custodian proposal into a parked route (dogfood lifeos-1): the policy
// withheld it, or an index proposal cannot be verified (no workload to
// measure it against). The schema guard records a parked remediation for
// the operator instead of a failure; other errors pass through.
func parkExpectedRefusal(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, executor.ErrCustodianProposalWithheld):
		return &schemaguard.ParkedRoute{Reason: "withheld by policy", Err: err}
	case errors.Is(err, executor.ErrVerificationUnavailable):
		return &schemaguard.ParkedRoute{Reason: "cannot be verified yet", Err: err}
	}
	return err
}

// parkedLog logs each parked proposal once per target and reason.
type parkedLog struct {
	mu   sync.Mutex
	seen map[string]string // target -> last reason logged
	log  func(format string, args ...any)
}

func newParkedLog(log func(format string, args ...any)) *parkedLog {
	return &parkedLog{seen: map[string]string{}, log: log}
}

func (p *parkedLog) note(target, reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[target] == reason {
		return
	}
	p.seen[target] = reason
	p.log("custodian proposal for %s parked: %s", target, reason)
}

// parked applies parkExpectedRefusal and logs a parked route once.
func (r executorProposalRouter) parked(targets []string, err error) error {
	err = parkExpectedRefusal(err)
	var p *schemaguard.ParkedRoute
	if errors.As(err, &p) {
		r.parkLog.note(strings.Join(targets, ","), p.Error())
	}
	return err
}
