package earned

import (
	"context"
	"time"
)

// evidenceSet is every read behind the evidence of a scope's pairs, each
// one set-based statement: the Trust view reads it once for the whole
// grid, a pair's Evidence reads it for that pair (the same statements,
// filtered), so the two always agree.
type evidenceSet struct {
	at       time.Time
	bench    map[Family]*EvalRun
	gameDays []EvalRun
	shadow   map[Family]Shadow
	live     map[pairKey]Live
	safety   map[Family]familySafetyRow
	records  map[pairKey]ClassRecord
	// safetyRead is true when safety holds every incident family's record.
	safetyRead bool
}

// evidenceNeeds says which reads a scope needs: the incident evidence
// (bench, game days, shadow reviews, live record, family safety) and the
// class records (verdict counts, the self-initiated evidence).
type evidenceNeeds struct{ incident, records bool }

// loadEvidence reads the evidence of pairs at now: the class records of
// every pair, the incident evidence of the incident pairs.
func (s *Service) loadEvidence(ctx context.Context, pairs []pairKey, need evidenceNeeds,
	now time.Time) (evidenceSet, error) {
	e := newEvidenceSet(now)
	if err := s.queueEvidence(ctx, s.store, pairs, need, e); err != nil {
		return evidenceSet{}, err
	}
	return *e, nil
}

func newEvidenceSet(now time.Time) *evidenceSet {
	return &evidenceSet{at: now, bench: map[Family]*EvalRun{}, shadow: map[Family]Shadow{},
		live: map[pairKey]Live{}, safety: map[Family]familySafetyRow{},
		records: map[pairKey]ClassRecord{}}
}

// queueEvidence runs (or queues on r) every read of pairs' evidence into
// e; e is complete once r has run.
func (s *Service) queueEvidence(ctx context.Context, r reads, pairs []pairKey,
	need evidenceNeeds, e *evidenceSet) error {
	if need.records {
		if err := s.store.readClassRecords(ctx, r, pairs, e.records); err != nil {
			return err
		}
	}
	incident := incidentPairs(pairs)
	if !need.incident || len(incident) == 0 {
		return nil
	}
	e.safetyRead = true
	names := familiesOf(incident)
	families := make([]Family, 0, len(names))
	for _, f := range names {
		families = append(families, Family(f))
	}
	th := s.cfg.Thresholds
	for _, read := range []func() error{
		func() error { return s.store.readBench(ctx, r, families, e.bench) },
		func() error {
			return s.store.readGameDays(ctx, r, e.at.Add(-th.BenchMaxAge), &e.gameDays)
		},
		func() error {
			return s.store.readShadow(ctx, r, names, e.at.Add(-th.ShadowDuration), e.shadow)
		},
		func() error { return s.store.readLive(ctx, r, incident, e.live) },
		func() error {
			return s.store.readSafety(ctx, r, names, e.at.Add(-s.cfg.SafetyWindow), e.safety)
		},
	} {
		if err := read(); err != nil {
			return err
		}
	}
	return nil
}

// evidence is one pair's evidence from the set.
func (s *Service) evidenceOf(e evidenceSet, f Family, c ActionClass) Evidence {
	ev := Evidence{Family: f, Class: c, At: e.at}
	if IsSelfInitiated(f) {
		rec := e.records[pairKey{f, c}]
		ev.Record, ev.Floor = &rec, s.floorStatus(c, e.at)
		return ev
	}
	ev.Bench, ev.GameDays = e.bench[f], e.gameDays
	ev.Shadow, ev.Live = e.shadow[f], e.live[pairKey{f, c}]
	if sf := e.safety[f]; sf.n > 0 {
		ev.FamilyViolations = sf.n
		clears := sf.last.Add(s.cfg.SafetyWindow)
		ev.ViolationsClearAt = &clears
	}
	return ev
}

// Evidence collects everything behind a promotion of the pair now.
func (s *Service) Evidence(ctx context.Context, f Family, c ActionClass) (Evidence, error) {
	now := s.now()
	self := IsSelfInitiated(f)
	e, err := s.loadEvidence(ctx, []pairKey{{f, c}},
		evidenceNeeds{incident: !self, records: self}, now)
	if err != nil {
		return Evidence{}, err
	}
	return s.evidenceOf(e, f, c), nil
}

// incidentPairs are the pairs of incident families.
func incidentPairs(pairs []pairKey) []pairKey {
	var out []pairKey
	for _, p := range pairs {
		if !IsSelfInitiated(p.family) {
			out = append(out, p)
		}
	}
	return out
}

// gridPairs is every family x class pair of the Trust view, in order.
func gridPairs() []pairKey {
	var out []pairKey
	for _, f := range AllFamilies() {
		for _, c := range ApplicableClasses(f) {
			out = append(out, pairKey{f, c})
		}
	}
	return out
}
