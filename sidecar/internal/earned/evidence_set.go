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
}

// evidenceNeeds says which reads a scope needs: the incident evidence
// (bench, game days, shadow reviews, live record, family safety) and the
// class records (verdict counts, the self-initiated evidence).
type evidenceNeeds struct{ incident, records bool }

// loadEvidence reads the evidence of pairs at now: the class records of
// every pair, the incident evidence of the incident pairs.
func (s *Service) loadEvidence(ctx context.Context, pairs []pairKey, need evidenceNeeds,
	now time.Time) (evidenceSet, error) {
	e := evidenceSet{at: now}
	var err error
	if need.records {
		if e.records, err = s.store.classRecordSet(ctx, pairs); err != nil {
			return evidenceSet{}, err
		}
	}
	incident := incidentPairs(pairs)
	if !need.incident || len(incident) == 0 {
		return e, nil
	}
	names := familiesOf(incident)
	families := make([]Family, 0, len(names))
	for _, f := range names {
		families = append(families, Family(f))
	}
	if e.bench, err = s.store.benchSet(ctx, families); err != nil {
		return evidenceSet{}, err
	}
	if e.gameDays, err = s.store.GameDayRuns(ctx,
		now.Add(-s.cfg.Thresholds.BenchMaxAge)); err != nil {
		return evidenceSet{}, err
	}
	th := s.cfg.Thresholds
	if e.shadow, err = s.store.shadowSet(ctx, names,
		now.Add(-th.ShadowDuration)); err != nil {
		return evidenceSet{}, err
	}
	if e.live, err = s.store.liveSet(ctx, incident); err != nil {
		return evidenceSet{}, err
	}
	e.safety, err = s.store.safetySet(ctx, names, now.Add(-s.cfg.SafetyWindow))
	if err != nil {
		return evidenceSet{}, err
	}
	return e, nil
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
