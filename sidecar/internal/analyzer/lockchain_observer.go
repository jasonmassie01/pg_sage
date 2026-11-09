package analyzer

import (
	"fmt"
	"sync"
	"time"
)

// QueuedDDL reports whether a lock chain is the queue behind an in-flight
// DDL the migration detector tracks, and that DDL's pid (migration-05).
// cmd wires it to the detector's tracked set.
type QueuedDDL interface {
	QueuedDDLPID(c LockChain) (pid int, ok bool)
}

// LockChainObserver keeps one database's latest lock-chain observation,
// recorded by the RCA fast path and the analyzer cycle, for the migration
// detector. An observation is fresh for two fast-path intervals; with the
// fast path disabled (interval 0) nothing is ever fresh. Nil is safe.
type LockChainObserver struct {
	mu       sync.Mutex
	chains   []LockChain
	at       time.Time
	interval time.Duration
	queued   QueuedDDL
	nudge    func()
	now      func() time.Time
}

// NewLockChainObserver returns an observer for a fast path that runs
// every fastPathInterval.
func NewLockChainObserver(fastPathInterval time.Duration) *LockChainObserver {
	return &LockChainObserver{interval: fastPathInterval, now: time.Now}
}

// SetQueuedDDL attaches the migration detector's tracked DDL set.
func (o *LockChainObserver) SetQueuedDDL(q QueuedDDL) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.queued = q
	o.mu.Unlock()
}

// SetNudger attaches the fast path's rate-limited nudge.
func (o *LockChainObserver) SetNudger(f func()) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.nudge = f
	o.mu.Unlock()
}

// Nudge asks the fast path to observe soon (it rate-limits).
func (o *LockChainObserver) Nudge() {
	if o == nil {
		return
	}
	o.mu.Lock()
	f := o.nudge
	o.mu.Unlock()
	if f != nil {
		f()
	}
}

// Record stores an observation (an empty one included: "no queue").
func (o *LockChainObserver) Record(chains []LockChain) {
	if o == nil {
		return
	}
	cp := copyChains(chains)
	o.mu.Lock()
	o.chains, o.at = cp, o.now()
	o.mu.Unlock()
}

// Fresh returns a copy of the latest observation and its time when it is
// at most two fast-path intervals old.
func (o *LockChainObserver) Fresh() ([]LockChain, time.Time, bool) {
	if o == nil {
		return nil, time.Time{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.interval <= 0 || o.at.IsZero() || o.now().Sub(o.at) > 2*o.interval {
		return nil, time.Time{}, false
	}
	return copyChains(o.chains), o.at, true
}

func (o *LockChainObserver) queuedDDL() QueuedDDL {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.queued
}

func copyChains(in []LockChain) []LockChain {
	out := make([]LockChain, len(in))
	for i, c := range in {
		c.BlockedPIDs = append([]int(nil), c.BlockedPIDs...)
		c.LockedRelations = append([]string(nil), c.LockedRelations...)
		waiters := make(map[int]int, len(c.DirectWaiters))
		for pid, n := range c.DirectWaiters {
			waiters[pid] = n
		}
		c.DirectWaiters = waiters
		out[i] = c
	}
	return out
}

// redirectQueuedDDL rewrites the finding of each chain that is the queue
// behind a tracked DDL: signalling the root reader is the wrong remedy
// there. The finding keeps its evidence and severity, drops the signal
// SQL and points to the DDL's migration_safety finding, which carries the
// approval-only cancel of the DDL.
func redirectQueuedDDL(findings []Finding, chains []LockChain, q QueuedDDL) {
	if q == nil {
		return
	}
	byRoot := make(map[string]LockChain, len(chains))
	for _, c := range chains {
		byRoot[fmt.Sprintf("pid:%d", c.RootBlockerPID)] = c
	}
	for i := range findings {
		f := &findings[i]
		c, ok := byRoot[f.ObjectIdentifier]
		if f.Category != "lock_chain" || !ok {
			continue
		}
		pid, queued := q.QueuedDDLPID(c)
		if !queued {
			continue
		}
		f.RecommendedSQL, f.RollbackSQL, f.ActionRisk = "", "", ""
		f.Detail["queued_ddl_pid"] = pid
		f.Recommendation = fmt.Sprintf("Sessions are queued behind in-flight DDL "+
			"pid %d, which waits for PID %d's lock: do not cancel the reader. See the "+
			"migration_safety finding for DDL pid %d (approval-only cancel, then retry "+
			"with lock_timeout).", pid, c.RootBlockerPID, pid)
	}
}
