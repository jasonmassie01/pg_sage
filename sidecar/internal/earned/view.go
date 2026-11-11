package earned

import (
	"context"
	"time"
)

// ClassView is one family x class row of the autonomy view.
type ClassView struct {
	Class         ActionClass   `json:"class"`
	Reversibility Reversibility `json:"reversibility"`
	Cap           Level         `json:"cap"`
	Granted       Level         `json:"granted"`
	Supported     Level         `json:"supported"`
	// Effective is the level the gate would allow now on the viewed
	// database (set only when the view is for a database).
	Effective  *Level      `json:"effective,omitempty"`
	Downgrades []Downgrade `json:"downgrades,omitempty"`
	Version    int64       `json:"version"`
	ChangedBy  string      `json:"changed_by,omitempty"`
	ChangedAt  *time.Time  `json:"changed_at,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Pending    *Proposal   `json:"pending,omitempty"`
	// Next is the evidence held against the next level, below the cap.
	Next *Assessment `json:"next,omitempty"`
	Live Live        `json:"live"`
	// Provenance is carried_over for a level kept from the pre-M7 policy
	// (CarriedRef names the decision), else ledger.
	Provenance string `json:"provenance"`
	CarriedRef string `json:"carried_ref,omitempty"`
	// HoldsUntil is when the evidence behind an earned level of L2 or more
	// stops supporting it unless new evidence arrives; ExpiringChecks are
	// the checks that expire then (earned-06).
	HoldsUntil     *time.Time `json:"holds_until,omitempty"`
	ExpiringChecks []string   `json:"expiring_checks,omitempty"`
	// ServerMajor and MajorCap are as on a Trust row (earned-07).
	ServerMajor *int      `json:"server_major,omitempty"`
	MajorCap    *MajorCap `json:"major_cap,omitempty"`
}

// FamilyView is one family's rows with its shadow record.
type FamilyView struct {
	Family Family `json:"family"`
	Shadow Shadow `json:"shadow"`
	// Bench is the report the family's bench checks read, with its
	// provenance (none without one).
	Bench   *BenchSummary `json:"bench,omitempty"`
	Classes []ClassView   `json:"classes"`
}

// View is the whole ledger.
type View struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Thresholds  Thresholds   `json:"-"`
	Bench       *EvalRun     `json:"bench,omitempty"`
	GameDays    []EvalRun    `json:"game_days"`
	Families    []FamilyView `json:"families"`
	// safety is the incident families' safety record the view read, for
	// Annotate to reuse; nil when the view did not read it.
	safety map[Family]familySafetyRow
}

// View lists every applicable pair with its level, cap, the level its
// evidence supports, its pending proposal and the next level's evidence:
// the proposal expiry, then every read in one pipelined round trip.
func (s *Service) View(ctx context.Context) (View, error) {
	if err := s.expire(ctx); err != nil {
		return View{}, err
	}
	now, b := s.now(), &readBatch{}
	in, err := s.queueLedger(ctx, b, viewPairs(),
		evidenceNeeds{incident: true, expiry: true}, now)
	if err != nil {
		return View{}, err
	}
	latest := map[Family]*EvalRun{}
	if err := s.store.readBench(ctx, b, []Family{""}, latest); err != nil {
		return View{}, err
	}
	if err := s.store.runBatch(ctx, b); err != nil {
		return View{}, err
	}
	return s.buildView(now, in, latest[""]), nil
}

// buildView assembles the view from its reads; bench is the newest report
// of any family. The game days are the evidence's own read (the same
// statement and window as GameDayRuns).
func (s *Service) buildView(now time.Time, in *ledgerReads, bench *EvalRun) View {
	pending, e := in.pendingByPair(), *in.e
	v := View{GeneratedAt: now, Thresholds: s.cfg.Thresholds, Bench: bench,
		GameDays: []EvalRun{}}
	if e.safetyRead {
		v.safety = e.safety
	}
	if len(e.gameDays) > 0 {
		v.GameDays = e.gameDays
	}
	for _, f := range Families() {
		fv := FamilyView{Family: f, Classes: []ClassView{}}
		for _, c := range ApplicableClasses(f) {
			ev := s.evidenceOf(e, f, c)
			fv.Shadow, fv.Bench = ev.Shadow, SummarizeBench(ev.Bench)
			st := in.granted(f, c)
			row := s.classView(st, ev, pending[pairKey{f, c}])
			row.HoldsUntil, row.ExpiringChecks = s.holding(st, row.Supported, ev, e.marks[f])
			fv.Classes = append(fv.Classes, row)
		}
		v.Families = append(v.Families, fv)
	}
	return v
}

// classView is one pair's row from its granted state and evidence.
func (s *Service) classView(st State, ev Evidence, pending *Proposal) ClassView {
	spec, _ := Spec(ev.Class)
	row := ClassView{Class: ev.Class, Reversibility: spec.Reversibility,
		Cap:     CapForPair(ev.Family, ev.Class),
		Granted: st.Level, Supported: SupportedLevel(s.cfg.Thresholds, ev),
		Version: st.Version, ChangedBy: st.ChangedBy, Reason: st.Reason,
		Pending: pending, Live: ev.Live, Provenance: st.Provenance,
		CarriedRef: st.CarriedRef, ServerMajor: st.ServerMajor}
	if st.Stored {
		at := st.ChangedAt
		row.ChangedAt = &at
	}
	if next := st.Level + 1; next <= row.Cap && next.Grantable() {
		a := Assess(s.cfg.Thresholds, next, ev)
		row.Next = &a
	}
	return row
}
