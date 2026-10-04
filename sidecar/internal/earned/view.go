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
}

// View lists every applicable pair with its level, cap, the level its
// evidence supports, its pending proposal and the next level's evidence.
func (s *Service) View(ctx context.Context) (View, error) {
	pending, err := s.PendingProposals(ctx)
	if err != nil {
		return View{}, err
	}
	byPair := map[pairKey]*Proposal{}
	for i := range pending {
		byPair[pairKey{pending[i].Family, pending[i].Class}] = &pending[i]
	}
	v := View{GeneratedAt: s.now(), Thresholds: s.cfg.Thresholds, GameDays: []EvalRun{}}
	for _, f := range Families() {
		fv, err := s.familyView(ctx, f, byPair)
		if err != nil {
			return View{}, err
		}
		v.Families = append(v.Families, fv)
	}
	if v.Bench, err = s.store.LatestBench(ctx, ""); err != nil {
		return View{}, err
	}
	runs, err := s.store.GameDayRuns(ctx, s.now().Add(-s.cfg.Thresholds.BenchMaxAge))
	if runs != nil {
		v.GameDays = runs
	}
	return v, err
}

func (s *Service) familyView(ctx context.Context, f Family,
	pending map[pairKey]*Proposal) (FamilyView, error) {
	fv := FamilyView{Family: f, Classes: []ClassView{}}
	for _, c := range ApplicableClasses(f) {
		ev, err := s.Evidence(ctx, f, c)
		if err != nil {
			return FamilyView{}, err
		}
		fv.Shadow, fv.Bench = ev.Shadow, SummarizeBench(ev.Bench)
		row, err := s.classView(ctx, ev, pending[pairKey{f, c}])
		if err != nil {
			return FamilyView{}, err
		}
		fv.Classes = append(fv.Classes, row)
	}
	return fv, nil
}

func (s *Service) classView(ctx context.Context, ev Evidence, pending *Proposal) (ClassView,
	error) {
	st, err := s.Granted(ctx, ev.Family, ev.Class)
	if err != nil {
		return ClassView{}, err
	}
	spec, _ := Spec(ev.Class)
	row := ClassView{Class: ev.Class, Reversibility: spec.Reversibility,
		Cap:     CapForPair(ev.Family, ev.Class),
		Granted: st.Level, Supported: SupportedLevel(s.cfg.Thresholds, ev),
		Version: st.Version, ChangedBy: st.ChangedBy, Reason: st.Reason,
		Pending: pending, Live: ev.Live, Provenance: st.Provenance,
		CarriedRef: st.CarriedRef}
	if st.Stored {
		at := st.ChangedAt
		row.ChangedAt = &at
	}
	if next := st.Level + 1; next <= row.Cap && next.Grantable() {
		a := Assess(s.cfg.Thresholds, next, ev)
		row.Next = &a
	}
	return row, nil
}
