package selfconfig

import "time"

// Status is where a derived key stands.
type Status string

const (
	// StatusDefault: the product default is in force (no derivation differs).
	StatusDefault Status = "default"
	// StatusDerived: a promoted derived value is in force.
	StatusDerived Status = "derived"
	// StatusShadow: a new candidate soaks in shadow; the active value stays.
	StatusShadow Status = "shadow"
	// StatusPinned: an admin pinned the current value; evidence is ignored.
	StatusPinned Status = "pinned"
	// StatusOperator: the operator set the key; it is never derived.
	StatusOperator Status = "operator"
)

// Phase is when a derivation pass runs: restart-bound keys change only
// at startup.
type Phase string

const (
	PhaseStartup Phase = "startup"
	PhaseLive    Phase = "live"
)

// EventKind is one kind of derivation ledger entry.
type EventKind string

const (
	EventShadow      EventKind = "shadow"       // a new candidate entered shadow
	EventPromoted    EventKind = "promoted"     // the soak passed; the value is active
	EventApplied     EventKind = "applied"      // a restart-bound value took effect
	EventHeld        EventKind = "held"         // kept in shadow (or refused), with why
	EventCleared     EventKind = "cleared"      // the shadow was dropped
	EventOperatorSet EventKind = "operator_set" // the operator's value now wins
	EventResumed     EventKind = "resumed"      // the operator unset it; derivation resumes
	EventPinned      EventKind = "pinned"
	EventUnpinned    EventKind = "unpinned"
)

// MaxSoakSamples caps the soak samples kept per key.
const MaxSoakSamples = 200

// State is one database's derivation state of one key.
type State struct {
	Key    string
	Status Status
	// Value is in force in the runtime now.
	Value float64
	// Pending is the value a restart-bound key takes at the next start.
	Pending *float64
	// Active is the promoted derived value (nil: the default).
	Active        *float64
	Shadow        *float64
	ShadowSince   time.Time
	ShadowReason  string
	ShadowOutcome Outcome
	Samples       []float64
	Pinned        *float64
	PinnedBy      string
	PinnedAt      time.Time
	Operator      *float64
	// Note says why nothing is derived (missing evidence, empty bounds).
	Note        string
	Rule        string
	RuleVersion int
	Evidence    []Citation
	Bounds      Bounds
	UpdatedAt   time.Time
}

// Event is one ledger entry a step produced.
type Event struct {
	Kind     EventKind
	Value    *float64
	Previous *float64
	Reason   string
}

func (s *State) clearShadow() {
	s.Shadow, s.ShadowSince, s.ShadowReason, s.ShadowOutcome, s.Samples =
		nil, time.Time{}, "", "", nil
}

func (s *State) status() Status {
	switch {
	case s.Operator != nil:
		return StatusOperator
	case s.Pinned != nil:
		return StatusPinned
	case s.Shadow != nil:
		return StatusShadow
	case s.Active != nil:
		return StatusDerived
	}
	return StatusDefault
}

func ptr(v float64) *float64 { return &v }
