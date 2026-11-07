package sre

import (
	"context"
	"time"
)

// Stale resume (sre-02). freshObservations judges evidence only against
// the investigation's newest observation. A run resumed long after its
// steps were committed (a sidecar restart picking up an orphaned
// collecting or evaluating row, an operator resuming a paused one) would
// skip those steps and conclude now on evidence of any age. Before it
// collects, a claimed run therefore judges its stored evidence against
// the clock: when the newest dated row is more than MaxEvidenceSpread
// old, the run ends expired with failure code stale_evidence. Expired is
// terminal and not a conclusion, so no diagnosis is ever shown for that
// evidence; a re-investigation is a new attempt.

// evidenceAge is when an investigation's newest dated evidence was
// observed and how old it is at the time it is judged. newest is zero
// when no evidence carries a time.
type evidenceAge struct {
	newest time.Time
	age    time.Duration
}

// ageOfEvidence judges ev at now. Undated rows never count as newest;
// evidence dated after now (the sidecar's and the database's clocks
// disagree) is current, never a negative age.
func ageOfEvidence(ev []Evidence, now time.Time) evidenceAge {
	var a evidenceAge
	for _, e := range ev {
		if e.ObservedAt.After(a.newest) {
			a.newest = e.ObservedAt
		}
	}
	if !a.newest.IsZero() {
		a.age = max(now.Sub(a.newest), 0)
	}
	return a
}

// stale reports dated evidence older than MaxEvidenceSpread: exactly
// MaxEvidenceSpread old still describes the incident.
func (a evidenceAge) stale() bool {
	return !a.newest.IsZero() && a.age > MaxEvidenceSpread
}

// apply records the age on a summary; no dated evidence records nothing.
func (a evidenceAge) apply(s *Summary) {
	if a.newest.IsZero() {
		s.EvidenceNewestAt, s.EvidenceAgeS = nil, 0
		return
	}
	newest := a.newest.UTC()
	s.EvidenceNewestAt, s.EvidenceAgeS = &newest, int64(a.age/time.Second)
}

// expireStale ends a resumed run whose evidence is too old, without a
// probe or a model turn. The store fences it on the lease and records
// the system's transition on the event chain.
func (c *Coordinator) expireStale(ctx context.Context, lease Lease, age evidenceAge) error {
	c.logFn("INFO", "sre: investigation %s: stored evidence is %s old (over %s); "+
		"ending it expired (%s) instead of concluding on it", lease.InvestigationID,
		age.age.Round(time.Second), MaxEvidenceSpread, ReasonStaleEvidence)
	s := Summary{Reason: ReasonStaleEvidence}
	age.apply(&s)
	_, err := c.store.ExpireStale(ctx, lease, s)
	return err
}

// concludedAge is the age, at conclusion, of everything the run stored:
// the plan's steps, the runbook's reads and any read or probe of the
// model turn, which the evidence the diagnosis started from lacks.
func (c *Coordinator) concludedAge(ctx context.Context, lease Lease) (evidenceAge, error) {
	ev, err := c.store.Evidence(ctx, lease.Scope, lease.InvestigationID)
	if err != nil {
		return evidenceAge{}, err
	}
	return ageOfEvidence(ev, c.now()), nil
}
