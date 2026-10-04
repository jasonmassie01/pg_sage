package earned

import (
	"context"
	"time"
)

// The Trust view (roadmap 1.2): one database's ledger as the Trust page
// shows it, every family x class of both kinds (incident remediations and
// self-initiated classes) with its level, the evidence counts, the last
// change and why, and the path to the next level.

// TrustCounts are a pair's recorded outcomes by verdict.
type TrustCounts struct {
	Improved     int `json:"improved"`
	Neutral      int `json:"neutral"`
	Regressed    int `json:"regressed"`
	RolledBack   int `json:"rolled_back"`
	Rejected     int `json:"rejected"`
	Insufficient int `json:"insufficient"`
	Unverifiable int `json:"unverifiable"`
}

// TrustChange is a pair's last level change: the history event (Event)
// when there is one, else the stored row's change; zero for a default.
type TrustChange struct {
	At     *time.Time `json:"at,omitempty"`
	By     string     `json:"by,omitempty"`
	Event  EventType  `json:"event,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// TrustRow is one family x class of the Trust view.
type TrustRow struct {
	Family        Family        `json:"family"`
	Kind          string        `json:"kind"`
	Class         ActionClass   `json:"class"`
	OutcomeClass  string        `json:"outcome_class,omitempty"`
	Level         Level         `json:"level"`
	Effective     *Level        `json:"effective,omitempty"`
	Supported     *Level        `json:"supported,omitempty"`
	Cap           Level         `json:"cap"`
	Reversibility Reversibility `json:"reversibility"`
	Provenance    string        `json:"provenance"`
	ProvenanceRef string        `json:"provenance_ref,omitempty"`
	Evidence      TrustCounts   `json:"evidence"`
	LastChange    TrustChange   `json:"last_change"`
	Next          *Assessment   `json:"next,omitempty"`
	Pending       *Proposal     `json:"pending,omitempty"`
	Downgrades    []Downgrade   `json:"downgrades,omitempty"`
}

// TrustView is one database's Trust page. Floor is the observation floor
// of a reversible MODERATE class (each row's next level holds its own).
type TrustView struct {
	Database      string             `json:"database"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Floor         *FloorStatus       `json:"floor"`
	Grandfathered *GrandfatherReport `json:"grandfathered,omitempty"`
	Rows          []TrustRow         `json:"rows"`
}

// TrustView builds the database's Trust view with a constant number of
// set-based statements, however many pairs it shows.
func (s *Service) TrustView(ctx context.Context) (TrustView, error) {
	pending, err := s.PendingProposals(ctx)
	if err != nil {
		return TrustView{}, err
	}
	byPair := map[pairKey]*Proposal{}
	for i := range pending {
		byPair[pairKey{pending[i].Family, pending[i].Class}] = &pending[i]
	}
	changes, err := s.store.lastChanges(ctx)
	if err != nil {
		return TrustView{}, err
	}
	levels, err := s.store.levelSet(ctx)
	if err != nil {
		return TrustView{}, err
	}
	now := s.now()
	e, err := s.loadEvidence(ctx, pairScope{},
		evidenceNeeds{incident: true, records: true}, now)
	if err != nil {
		return TrustView{}, err
	}
	v := TrustView{Database: s.store.database, GeneratedAt: now,
		Floor: s.floorStatus(ClassIndexCreate, now), Rows: []TrustRow{}}
	if v.Grandfathered, err = s.Grandfathered(ctx); err != nil {
		return TrustView{}, err
	}
	for _, f := range AllFamilies() {
		for _, c := range ApplicableClasses(f) {
			key := pairKey{f, c}
			st, ok := levels[key]
			if !ok {
				st = defaultState(f, c)
			}
			v.Rows = append(v.Rows, s.trustRow(st, s.evidenceOf(e, f, c), e.records[key],
				byPair[key], changes[key]))
		}
	}
	return v, nil
}

func (s *Service) trustRow(st State, ev Evidence, rec ClassRecord, pending *Proposal,
	change Event) TrustRow {
	f, c := st.Family, st.Class
	spec, _ := Spec(c)
	row := TrustRow{Family: f, Kind: KindIncident, Class: c, Level: st.Level,
		Cap: CapForPair(f, c), Reversibility: spec.Reversibility, Provenance: st.Provenance,
		ProvenanceRef: st.CarriedRef, Evidence: countsOf(rec), Pending: pending,
		LastChange: lastChange(st, change)}
	if IsSelfInitiated(f) {
		row.Kind, row.OutcomeClass = KindSelfInitiated, OutcomeClassFor(c)
	} else {
		supported := SupportedLevel(s.cfg.Thresholds, ev)
		row.Supported = &supported
	}
	if next := st.Level + 1; next <= row.Cap && next.Grantable() {
		a := Assess(s.cfg.Thresholds, next, ev)
		row.Next = &a
	}
	return row
}

func countsOf(r ClassRecord) TrustCounts {
	return TrustCounts{Improved: r.Improved, Neutral: r.Neutral, Regressed: r.Regressed,
		RolledBack: r.RolledBack, Rejected: r.Rejected, Insufficient: r.Insufficient,
		Unverifiable: r.Unverifiable}
}

func lastChange(st State, e Event) TrustChange {
	if e.ID != 0 {
		return TrustChange{At: stamp(e.At), By: e.Actor, Event: e.Type, Reason: e.Reason}
	}
	if !st.Stored {
		return TrustChange{}
	}
	return TrustChange{At: stamp(st.ChangedAt), By: st.ChangedBy, Reason: st.Reason}
}

// AnnotateTrust fills each row's effective level on this limiter's
// database and the database-wide downgrade signals holding now: the
// error budget and HA role read once, every family's safety record in
// one statement.
func (l *Limiter) AnnotateTrust(ctx context.Context, v *TrustView) {
	self := l.selfDowngrades(ctx)
	safety, err := l.svc.store.safetySet(ctx, pairScope{},
		l.svc.now().Add(-l.svc.cfg.SafetyWindow))
	byFamily := map[Family][]Downgrade{}
	for i := range v.Rows {
		row := &v.Rows[i]
		downs := self
		if !IsSelfInitiated(row.Family) {
			if _, ok := byFamily[row.Family]; !ok {
				byFamily[row.Family] = append(append([]Downgrade(nil), self...),
					safetyDowngrades(safety[row.Family].n, err)...)
			}
			downs = byFamily[row.Family]
		}
		level := MinLevel(row.Level, row.Cap)
		switch {
		case row.Provenance == ProvenanceCarriedOver:
			level = MinLevel(row.Level, carryCap(row.Class))
		case row.Supported != nil && row.Level >= L2:
			level = MinLevel(level, *row.Supported)
		}
		if len(downs) > 0 && level > L1 {
			level = L1
		}
		if len(downs) > 0 {
			row.Downgrades = downs
		}
		row.Effective = &level
	}
}
