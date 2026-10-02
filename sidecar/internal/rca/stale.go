package rca

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Resolutions the engine records for incidents it stopped seeing
// (dogfood lifeos-1): not re-detected within rca.stale_after_hours, the
// backend they name is gone, or merged into the earliest open incident
// of the same identity at hydration.
const (
	ResolvedByStale       = "pg_sage:stale"
	ResolvedBySubjectGone = "pg_sage:subject_gone"
	ResolvedByMerged      = "pg_sage:merged"
)

// BackendRef names one backend: its pid and, when known, backend_start
// (pids are reused).
type BackendRef struct {
	PID          int
	BackendStart time.Time
}

// holderEvidence is the vacuum_blocked holder link's evidence, the only
// subject legacy rows carry.
var holderEvidence = regexp.MustCompile(`^PID ([0-9]{1,10}) in state: idle in transaction$`)

// backendSubject returns the backend an incident is about: the session
// holding the oldest xmin of a vacuum_blocked incident. Lock-chain
// incidents are about the contention, not one blocker, and have none.
func backendSubject(inc *Incident) (BackendRef, bool) {
	for _, l := range inc.CausalChain {
		if l.Signal != "vacuum_blocked" {
			continue
		}
		if b := l.Blocker; b != nil && b.PID > 0 {
			return BackendRef{PID: b.PID, BackendStart: b.BackendStart}, true
		}
		m := holderEvidence.FindStringSubmatch(l.Evidence)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid <= 0 || pid > 1<<31-1 {
			continue
		}
		return BackendRef{PID: pid}, true
	}
	return BackendRef{}, false
}

// backendGone reports a backend that no longer exists: its pid is not
// listed, or is listed with a different backend_start (reused). A pid
// whose start is unknown on either side counts as present.
func backendGone(ref BackendRef, live map[int]*time.Time) bool {
	start, ok := live[ref.PID]
	if !ok {
		return true
	}
	if start == nil || ref.BackendStart.IsZero() {
		return false
	}
	return !start.Truncate(time.Microsecond).Equal(ref.BackendStart.Truncate(time.Microsecond))
}

// BackendLookup lists which of pids exist, with their backend_start (nil
// when not visible to this role).
type BackendLookup func(ctx context.Context, pids []int) (map[int]*time.Time, error)

const liveBackendsSQL = `/* pg_sage */
SELECT pid, backend_start FROM pg_catalog.pg_stat_activity WHERE pid = ANY($1)`

// lookupBackends is the default lookup against the engine's store.
func (e *Engine) lookupBackends(ctx context.Context, pids []int) (map[int]*time.Time,
	error) {
	if e.store == nil {
		return nil, fmt.Errorf("no incident store bound")
	}
	rows, err := e.store.Query(ctx, liveBackendsSQL, pids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]*time.Time, len(pids))
	for rows.Next() {
		var pid int
		var start *time.Time
		if err := rows.Scan(&pid, &start); err != nil {
			return nil, err
		}
		out[pid] = start
	}
	return out, rows.Err()
}

// resolveGoneSubjects resolves open incidents whose backend is gone. It
// runs before the cycle's detections merge, so a new holder opens its
// own incident. Without a store (or when the lookup fails) nothing is
// resolved.
func (e *Engine) resolveGoneSubjects(ctx context.Context) {
	e.mu.Lock()
	subjects := map[string]BackendRef{}
	var pids []int
	for i := range e.incidents {
		inc := &e.incidents[i]
		if ref, ok := backendSubject(inc); ok && inc.ResolvedAt == nil {
			subjects[inc.ID] = ref
			pids = append(pids, ref.PID)
		}
	}
	lookup := e.backendLookup
	if lookup == nil && e.store != nil {
		lookup = e.lookupBackends
	}
	e.mu.Unlock()
	if len(pids) == 0 || lookup == nil {
		return
	}
	live, err := lookup(ctx, pids)
	if err != nil {
		e.logFn("warn", "rca: listing backends named by %d open incidents failed; "+
			"none resolved this cycle: %v", len(pids), err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.incidents {
		inc := &e.incidents[i]
		ref, ok := subjects[inc.ID]
		if !ok || inc.ResolvedAt != nil || !backendGone(ref, live) {
			continue
		}
		e.resolveInMemory(inc, ResolvedBySubjectGone, fmt.Sprintf(
			"backend %d is gone (no longer in pg_stat_activity)", ref.PID))
	}
}

// resolveStale resolves open incidents not re-detected within the stale
// window. Caller holds e.mu.
func (e *Engine) resolveStale(now time.Time) {
	window := e.cfg.StaleAfter()
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt != nil {
			continue
		}
		last := lastSeen(inc)
		if now.Sub(last) <= window {
			continue
		}
		e.resolveInMemory(inc, ResolvedByStale, fmt.Sprintf(
			"stale: not re-detected for more than %s (last detected %s)",
			window, last.UTC().Format(time.RFC3339)))
	}
}

func lastSeen(inc *Incident) time.Time {
	if inc.LastDetectedAt.IsZero() {
		return inc.DetectedAt
	}
	return inc.LastDetectedAt
}

// xminHolder is the structured identity of the idle-in-transaction
// session a vacuum_blocked incident names, so the incident can resolve
// when that session ends.
func xminHolder(l collector.LockInfo) *BlockerIdentity {
	b := &BlockerIdentity{PID: l.PID, State: "idle in transaction"}
	if l.BackendStart != nil {
		b.BackendStart = *l.BackendStart
	}
	return b
}
