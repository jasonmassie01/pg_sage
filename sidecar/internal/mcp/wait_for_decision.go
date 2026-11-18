package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// wait_for_decision (mcp-01b) waits on the process's live-event stream,
// never on a polling loop of its own: it reads the item once, then again
// only when the stream says something changed. It returns at the item's
// next state (or at once when the item is final), when the timeout passes
// (state as last read, next wait_for_decision), or when the stream shuts
// down (state unknown, next get_request_status). A waiter holds no
// database connection while it waits, and waiters are capped per
// principal and in total so waiting agents cannot exhaust the process.

// Wait bounds.
const (
	MaxWaitSeconds         = 60
	defaultWaitSeconds     = 30
	MaxWaitersPerPrincipal = 5
	MaxWaitersTotal        = 200
	// deadlineMargin ends a wait this long before the caller's own
	// deadline, so the caller gets an answer rather than a timeout.
	deadlineMargin = 250 * time.Millisecond
)

const stateUnknown = "unknown"

// DecisionEvents is the live-event stream: each value on the channel says
// something may have changed; the channel closes when the stream shuts
// down. The cancel function ends the subscription.
type DecisionEvents interface {
	Subscribe() (<-chan struct{}, func())
}

// WithDecisionEvents sets the stream wait_for_decision waits on; without
// one, wait_for_decision is unavailable and get_request_status still works.
func (s *Server) WithDecisionEvents(events DecisionEvents) *Server {
	s.events = events
	return s
}

// waitTimeout is timeout_seconds: absent is the default, else a whole
// number from 1 to MaxWaitSeconds.
func waitTimeout(raw json.RawMessage) (time.Duration, bool) {
	if len(raw) == 0 {
		return defaultWaitSeconds * time.Second, true
	}
	var seconds int
	if json.Unmarshal(raw, &seconds) != nil || string(raw) == "null" ||
		seconds < 1 || seconds > MaxWaitSeconds {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func (s *Server) waitForDecision(ctx context.Context, q RequestQuery,
	timeout time.Duration) (any, *rpcError) {
	if s.events == nil {
		return nil, failure(codeUnavailable, "the live-event stream is not running here; "+
			"call get_request_status instead")
	}
	deadline := waitDeadline(ctx, timeout)
	release, failed := s.waiters.acquire(waiterKey(ctx), deadline)
	if failed != nil {
		return nil, failed
	}
	defer release()
	events, unsubscribe := s.events.Subscribe() // before the read: no change is missed
	defer unsubscribe()
	first, failed := s.ownedStatus(ctx, q)
	if failed != nil {
		return nil, failed
	}
	if first.Final {
		return statusResult(q, first), nil
	}
	return s.awaitChange(ctx, q, first, events, time.Until(deadline))
}

// awaitChange returns at the first read whose state differs from first's.
func (s *Server) awaitChange(ctx context.Context, q RequestQuery, first RequestRecord,
	events <-chan struct{}, wait time.Duration) (any, *rpcError) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, failure(codeCancelled, "wait cancelled")
		case <-timer.C:
			return statusResult(q, first), nil
		case _, open := <-events:
			if !open {
				return statusResult(q, unknownRecord("the live-event stream shut down")), nil
			}
			now, failed := s.ownedStatus(ctx, q)
			switch {
			case failed != nil && failed.Code == codeNotFound:
				return nil, failed
			case failed != nil:
				return statusResult(q, unknownRecord("the status could not be read")), nil
			case now.State != first.State || now.Final:
				return statusResult(q, now), nil
			}
		}
	}
}

func unknownRecord(why string) RequestRecord {
	return RequestRecord{State: stateUnknown, Outcome: why + "; call get_request_status"}
}

// waitDeadline is now+timeout, or earlier when the caller's context ends
// sooner (a stdio call timeout, a closing HTTP request).
func waitDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if own, ok := ctx.Deadline(); ok && own.Add(-deadlineMargin).Before(deadline) {
		deadline = own.Add(-deadlineMargin)
	}
	return deadline
}

// waiterKey is whom a waiter counts against: the agent principal, else the
// actor. An unbound caller never owns an item, so its key does not matter.
func waiterKey(ctx context.Context) string {
	p, _ := PrincipalFromContext(ctx)
	if p.PrincipalID != "" {
		return "principal:" + p.PrincipalID
	}
	return "actor:" + p.Actor
}

// waiterPool caps waiters per principal and in total. It records each
// waiter's deadline so a refusal says when a slot frees up.
type waiterPool struct {
	mu   sync.Mutex
	next int
	by   map[string]map[int]time.Time
	all  int
}

func newWaiterPool() *waiterPool {
	return &waiterPool{by: map[string]map[int]time.Time{}}
}

func (p *waiterPool) acquire(key string, deadline time.Time) (func(), *rpcError) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case len(p.by[key]) >= MaxWaitersPerPrincipal:
		return nil, waitersFull(fmt.Sprintf("you already have %d waits open",
			MaxWaitersPerPrincipal), p.soonest(p.by[key]))
	case p.all >= MaxWaitersTotal:
		return nil, waitersFull(fmt.Sprintf("the server already has %d waits open",
			MaxWaitersTotal), p.soonestOverall())
	}
	if p.by[key] == nil {
		p.by[key] = map[int]time.Time{}
	}
	p.next++
	id := p.next
	p.by[key][id] = deadline
	p.all++
	var once sync.Once
	return func() { once.Do(func() { p.release(key, id) }) }, nil
}

func (p *waiterPool) release(key string, id int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.by[key], id)
	if len(p.by[key]) == 0 {
		delete(p.by, key)
	}
	p.all--
}

// soonest is when the first of deadlines passes.
func (p *waiterPool) soonest(deadlines map[int]time.Time) time.Duration {
	times := make([]time.Time, 0, len(deadlines))
	for _, d := range deadlines {
		times = append(times, d)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	if len(times) == 0 {
		return time.Second
	}
	return time.Until(times[0])
}

func (p *waiterPool) soonestOverall() time.Duration {
	merged := map[int]time.Time{}
	for _, deadlines := range p.by {
		for id, d := range deadlines {
			merged[id] = d
		}
	}
	return p.soonest(merged)
}

func waitersFull(why string, retry time.Duration) *rpcError {
	return failure(codeRateLimited, "rate limited: "+why+"; wait for one to return or "+
		"call get_request_status").withRetryAfter(max(retry, time.Second))
}
