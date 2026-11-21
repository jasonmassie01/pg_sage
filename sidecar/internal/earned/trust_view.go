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
	// Shadow scores the ledger counts (roadmap 1.4).
	ShadowCorrect   int `json:"shadow_correct"`
	ShadowIncorrect int `json:"shadow_incorrect"`
	ShadowNeutral   int `json:"shadow_neutral"`
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
	// Bench is the report an incident pair's bench checks read (the
	// newest that counts, whatever its origin), with its provenance.
	Bench *BenchSummary `json:"bench,omitempty"`
	// HoldsUntil and ExpiringChecks are when an earned level's evidence
	// expires and which checks expire then (ClassView, earned-06).
	HoldsUntil     *time.Time `json:"holds_until,omitempty"`
	ExpiringChecks []string   `json:"expiring_checks,omitempty"`
	// ServerMajor is the PostgreSQL major the level was granted on;
	// MajorCap, set by AnnotateTrust, a self-initiated L3 capped at L2
	// because the server now runs another (earned-07).
	ServerMajor *int      `json:"server_major,omitempty"`
	MajorCap    *MajorCap `json:"major_cap,omitempty"`
	// ApprovalHistory is a self-initiated class's person approvals in the
	// window (ux-07a); nil for an incident class.
	ApprovalHistory *ApprovalHistory `json:"approval_history,omitempty"`
	// Mode and ModeBecause, set by AnnotateMode, are the class's step on
	// the autonomy scale and why (ux-06b); empty until annotated.
	Mode        ModeName `json:"mode,omitempty"`
	ModeBecause []string `json:"mode_because,omitempty"`
}

// TrustView is one database's Trust page. Floor is the observation floor
// of a reversible MODERATE class (each row's next level holds its own).
type TrustView struct {
	Database      string             `json:"database"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Floor         *FloorStatus       `json:"floor"`
	Grandfathered *GrandfatherReport `json:"grandfathered,omitempty"`
	Rows          []TrustRow         `json:"rows"`
	// safety is the incident families' safety record the view read, for
	// AnnotateTrust to reuse; nil when the view did not read it.
	safety map[Family]familySafetyRow
}

// trustReads is everything a Trust view reads, filled by one batch.
type trustReads struct {
	pending []Proposal
	changes map[pairKey]Event
	levels  []State
	grand   *GrandfatherReport
	e       *evidenceSet
}

// TrustView builds the database's Trust view: the proposal expiry, then
// every read in one pipelined round trip, however many pairs it shows.
func (s *Service) TrustView(ctx context.Context) (TrustView, error) {
	if err := s.expire(ctx); err != nil {
		return TrustView{}, err
	}
	grid := gridPairs()
	now := s.now()
	in := trustReads{changes: map[pairKey]Event{}, e: newEvidenceSet(now)}
	b := &readBatch{}
	for _, queue := range []func() error{
		func() error { return s.store.readProposals(ctx, b, StatusPending, 200, &in.pending) },
		func() error { return s.store.readLastChanges(ctx, b, grid, in.changes) },
		func() error { return s.store.readLevels(ctx, b, &in.levels) },
		func() error {
			return s.queueEvidence(ctx, b, grid,
				evidenceNeeds{incident: true, records: true, expiry: true, approvals: true},
				in.e)
		},
		func() error { return s.store.readGrandfather(ctx, b, &in.grand) },
	} {
		if err := queue(); err != nil {
			return TrustView{}, err
		}
	}
	if err := s.store.runBatch(ctx, b); err != nil {
		return TrustView{}, err
	}
	return s.buildTrustView(grid, now, in), nil
}

// buildTrustView assembles the view from its reads.
func (s *Service) buildTrustView(grid []pairKey, now time.Time, in trustReads) TrustView {
	byPair := map[pairKey]*Proposal{}
	for i := range in.pending {
		byPair[pairKey{in.pending[i].Family, in.pending[i].Class}] = &in.pending[i]
	}
	levels, e, changes := levelMap(in.levels), *in.e, in.changes
	v := TrustView{Database: s.store.database, GeneratedAt: now,
		Floor: s.floorStatus(ClassIndexCreate, now), Rows: []TrustRow{},
		Grandfathered: in.grand}
	if e.safetyRead {
		v.safety = e.safety
	}
	for _, key := range grid {
		st, ok := levels[key]
		if !ok {
			st = defaultState(key.family, key.class)
		}
		st = composedLevel(st, ok, func(p Pair) Level {
			if cs, ok := levels[pairKey{p.Family, p.Class}]; ok {
				return cs.Level
			}
			return defaultLevel(p.Family)
		})
		ev := s.evidenceOf(e, key.family, key.class)
		row := s.trustRow(st, ev, e.records[key], byPair[key], changes[key])
		if row.Supported != nil {
			row.HoldsUntil, row.ExpiringChecks = s.holding(st, *row.Supported, ev,
				e.marks[key.family])
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

func (s *Service) trustRow(st State, ev Evidence, rec ClassRecord, pending *Proposal,
	change Event) TrustRow {
	f, c := st.Family, st.Class
	spec, _ := Spec(c)
	row := TrustRow{Family: f, Kind: KindIncident, Class: c, Level: st.Level,
		Cap: CapForPair(f, c), Reversibility: spec.Reversibility, Provenance: st.Provenance,
		ProvenanceRef: st.CarriedRef, Evidence: countsOf(rec), Pending: pending,
		LastChange: lastChange(st, change), ServerMajor: st.ServerMajor}
	if IsSelfInitiated(f) {
		row.Kind, row.OutcomeClass = KindSelfInitiated, OutcomeClassFor(c)
		row.ApprovalHistory = ev.ApprovalHistory
	} else {
		supported := SupportedLevel(s.cfg.Thresholds, ev)
		row.Supported, row.Bench = &supported, SummarizeBench(ev.Bench)
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
		Unverifiable: r.Unverifiable, ShadowCorrect: r.ShadowCorrect,
		ShadowIncorrect: r.ShadowIncorrect, ShadowNeutral: r.ShadowNeutral}
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
// one statement (or reused from the view). A row without a supported
// level shows L0 where the evidence would bind (fail closed).
func (l *Limiter) AnnotateTrust(ctx context.Context, v *TrustView) {
	sig := l.viewSignals(ctx, v.safety, rowFamilies(v.Rows, false))
	mv := l.majorView(ctx)
	for i := range v.Rows {
		row := &v.Rows[i]
		downs := sig.of(row.Family)
		supported := L0
		if row.Supported != nil {
			supported = *row.Supported
		}
		st := State{Family: row.Family, Class: row.Class, Level: row.Level,
			Provenance: row.Provenance, ServerMajor: row.ServerMajor}
		level := viewLevel(st, supported, downs)
		if row.MajorCap = mv.capOf(ctx, st); row.MajorCap != nil {
			level = MinLevel(level, L2)
		}
		if len(downs) > 0 {
			row.Downgrades = downs
		}
		row.Effective = &level
	}
}

// rowFamilies lists the distinct families of rows, self-initiated ones
// only when self.
func rowFamilies(rows []TrustRow, self bool) []string {
	pairs := make([]pairKey, 0, len(rows))
	for _, r := range rows {
		if IsSelfInitiated(r.Family) == self {
			pairs = append(pairs, pairKey{r.Family, r.Class})
		}
	}
	return familiesOf(pairs)
}
