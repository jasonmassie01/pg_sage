package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrQuiesceTimeout reports that a drain deadline passed while actions were
// still executing; the caller cancels them.
var ErrQuiesceTimeout = errors.New("executor drain deadline passed with actions in flight")

// Quiesce drains the executor before its runtime is stopped. Every action
// (background, operator, custodian, rollback, verification) executes while
// holding a DDL slot, so Quiesce takes every slot: it returns once no
// action is executing, and from its first slot on no new action can start
// (a background action parks with ErrDDLSlotUnavailable and is retried by
// the next runtime generation). On ctx expiry it returns ErrQuiesceTimeout
// with the slots it holds. The returned release gives the slots back; it is
// idempotent and must be called once the runtime is cancelled.
func (e *Executor) Quiesce(ctx context.Context) (func(), error) {
	if e == nil || e.ddlSem == nil {
		return func() {}, nil
	}
	held := 0
	var once sync.Once
	release := func() {
		once.Do(func() {
			for range held {
				<-e.ddlSem
			}
			e.quiesced.Add(-int32(held))
		})
	}
	for held < cap(e.ddlSem) {
		if !e.takeQuiesceSlot(ctx) {
			return release, fmt.Errorf("%w: %d in-flight actions still running: %w",
				ErrQuiesceTimeout, cap(e.ddlSem)-held, ctx.Err())
		}
		held++
	}
	return release, nil
}

// takeQuiesceSlot prefers a free slot over a done context, so a cancelled
// drain still blocks every slot no action holds.
func (e *Executor) takeQuiesceSlot(ctx context.Context) bool {
	select {
	case e.ddlSem <- struct{}{}:
		e.quiesced.Add(1)
		return true
	default:
	}
	select {
	case e.ddlSem <- struct{}{}:
		e.quiesced.Add(1)
		return true
	case <-ctx.Done():
		return false
	}
}

// InFlightActions is the number of actions executing now, excluding slots
// a drain holds.
func (e *Executor) InFlightActions() int {
	if e == nil || e.ddlSem == nil {
		return 0
	}
	// The two reads are not atomic together; clamp a transient negative.
	return max(len(e.ddlSem)-int(e.quiesced.Load()), 0)
}
