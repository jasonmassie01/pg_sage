package impact

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// target is part i's relation context (nil when the part has none).
func (r *run) target(s *stmt, i int) *relation {
	if i >= len(s.targets) || s.targets[i] == "" {
		return nil
	}
	return r.rels[s.targets[i]]
}

// lockQueue is CR-02 for part i: the statements expected to queue behind
// its lock at the peak conflicting rate, and while it waits for the lock.
func (r *run) lockQueue(s *stmt, i int, as []assessment) {
	p := s.parts[i]
	mode := statementLock(p)
	rel := r.target(s, i)
	if _, blocks := conflictsWith[mode]; !blocks || rel == nil || !rel.found {
		return
	}
	// A new foreign key takes SHARE ROW EXCLUSIVE on the referenced table,
	// whatever the referencing side takes: only its writes queue.
	locks := []tableLock{{rel: rel.name, mode: mode}}
	for _, fk := range foreignKeys(&p) {
		locks = append(locks, tableLock{rel: qualified(*fk.def.RefRelation),
			mode: lockShareRowExclusive})
	}
	q := lockQueueData{Relation: rel.name, LockMode: mode, Method: "rule",
		ConflictingQueryIDs: []int64{}, LockTimeoutMs: p.LockTimeoutMs}
	ids := []string{s.evID, rel.evID}
	for _, ws := range r.conflicting(locks...) {
		q.ConflictingQueryIDs = append(q.ConflictingQueryIDs, ws.id)
		ids = append(ids, r.queryEvidence(ws, rel))
		if ws.rateKnown {
			q.PeakConflictingPerS += ws.rate.peak
		} else {
			q.RateUnknownIDs = append(q.RateUnknownIDs, ws.id)
		}
	}
	q.HoldMs, q.HoldSource = r.holdMs(s, i, mode, as), holdRiskAssessor
	if p.Kind == sqlast.DDLCreateIndex && rel.heapBytes > 0 {
		q.HoldMs, q.HoldSource = indexBuildHoldMs(rel.heapBytes), holdIndexBuild
	}
	q.Queued = queueEstimate(q.PeakConflictingPerS, q.HoldMs)
	q.WaitingExposure = waitingExposure(q.PeakConflictingPerS, p.LockTimeoutMs)
	q.NoLockTimeout = q.WaitingExposure == nil && len(q.ConflictingQueryIDs) > 0
	q.Formula = fmt.Sprintf("queued = %.3f conflicting calls/s x %.3f s hold = %.2f",
		q.PeakConflictingPerS, float64(q.HoldMs)/1000, q.Queued)
	id := r.addEvidence("lockq", partRef(s.index, i), fmt.Sprintf("%s on %s: about %.1f "+
		"statements queue", mode, rel.name, q.Queued), q)
	r.find(RuleLockQueue, lockQueueSeverity(q, r.cfg), lockQueueTitle(q),
		lockQueueText(q, r.work), append(ids, id)...)
}

// lockQueueSeverity is the queue's severity; with no conflicting rate
// known at all, nothing proves the queue is short: at least a warning.
func lockQueueSeverity(q lockQueueData, cfg config.ChangeReviewConfig) string {
	sev := queueSeverity(q.Queued, cfg)
	if sev == changereview.SeverityInfo && q.PeakConflictingPerS == 0 &&
		len(q.RateUnknownIDs) > 0 {
		return changereview.SeverityWarning
	}
	return sev
}

func lockQueueTitle(q lockQueueData) string {
	return fmt.Sprintf("About %.0f statements queue behind %s on %s", q.Queued, q.LockMode,
		q.Relation)
}

func lockQueueText(q lockQueueData, w *workload) string {
	t := fmt.Sprintf("%s blocks %d workload statements on %s at a peak of %.2f calls/s; "+
		"held about %d ms (%s, method rule), about %.2f statements queue behind it.",
		q.LockMode, len(q.ConflictingQueryIDs), q.Relation, q.PeakConflictingPerS, q.HoldMs,
		q.HoldSource, q.Queued)
	switch {
	case q.WaitingExposure != nil:
		t += fmt.Sprintf(" While it waits for the lock, at most %.1f statements queue "+
			"(lock_timeout %d ms).", *q.WaitingExposure, *q.LockTimeoutMs)
	case q.NoLockTimeout:
		t += " The script sets no lock_timeout: while the change waits behind a long " +
			"transaction, every conflicting statement queues behind it, unbounded. " +
			"SET lock_timeout before the DDL."
	}
	if len(q.RateUnknownIDs) > 0 {
		t += fmt.Sprintf(" %d conflicting statements have no query_store history: "+
			"their rate is unknown and not counted.", len(q.RateUnknownIDs))
	}
	if w != nil && !w.available {
		t += " The workload is unknown (" + w.reason + "): nothing proves no " +
			"statement queues."
	}
	return t
}

// severityFor is a warning-level finding raised to critical when crit.
func severityFor(crit bool) string {
	if crit {
		return changereview.SeverityCritical
	}
	return changereview.SeverityWarning
}
