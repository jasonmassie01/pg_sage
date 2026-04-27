package cases

import "time"

type SourceType string

const (
	SourceFindingType  SourceType = "finding"
	SourceIncidentType SourceType = "incident"
	SourceQueryType    SourceType = "query_hint"
	SourceForecastType SourceType = "forecast"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

type State string

const (
	StateOpen     State = "open"
	StateResolved State = "resolved"
	StateExpired  State = "expired"
)

type Evidence struct {
	Type    string         `json:"type"`
	Summary string         `json:"summary"`
	Detail  map[string]any `json:"detail,omitempty"`
}

type CaseInput struct {
	SourceType       SourceType
	SourceID         string
	DatabaseName     string
	IdentityKey      string
	Title            string
	Severity         Severity
	Why              string
	WhyNow           string
	Evidence         []Evidence
	ActionCandidates []ActionCandidate
	ObservedAt       time.Time
}

type Case struct {
	ID               string            `json:"id"`
	SourceType       SourceType        `json:"source_type"`
	SourceIDs        []string          `json:"source_ids"`
	DatabaseName     string            `json:"database_name"`
	IdentityKey      string            `json:"identity_key"`
	Title            string            `json:"title"`
	Severity         Severity          `json:"severity"`
	State            State             `json:"state"`
	Why              string            `json:"why"`
	WhyNow           string            `json:"why_now"`
	Evidence         []Evidence        `json:"evidence"`
	ObservedAt       time.Time         `json:"observed_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	ActionCandidates []ActionCandidate `json:"action_candidates,omitempty"`
	Actions          []CaseAction      `json:"actions,omitempty"`
}

type ActionCandidate struct {
	ActionType       string     `json:"action_type"`
	RiskTier         string     `json:"risk_tier"`
	Confidence       float64    `json:"confidence"`
	ProposedSQL      string     `json:"proposed_sql,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	OutputModes      []string   `json:"output_modes,omitempty"`
	RollbackClass    string     `json:"rollback_class,omitempty"`
	VerificationPlan []string   `json:"verification_plan,omitempty"`
}

type CaseAction struct {
	Type      string    `json:"type"`
	RiskTier  string    `json:"risk_tier"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

func NewCase(input CaseInput) Case {
	now := input.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	whyNow := input.WhyNow
	if whyNow == "" {
		whyNow = "not urgent"
	}

	return Case{
		ID:               input.IdentityKey,
		SourceType:       input.SourceType,
		SourceIDs:        []string{input.SourceID},
		DatabaseName:     input.DatabaseName,
		IdentityKey:      input.IdentityKey,
		Title:            input.Title,
		Severity:         input.Severity,
		State:            StateOpen,
		Why:              input.Why,
		WhyNow:           whyNow,
		Evidence:         input.Evidence,
		ActionCandidates: input.ActionCandidates,
		ObservedAt:       now,
		UpdatedAt:        now,
	}
}

type SourceFinding struct {
	ID               string
	DatabaseName     string
	Category         string
	Severity         Severity
	ObjectType       string
	ObjectIdentifier string
	RuleID           string
	Title            string
	Recommendation   string
	RecommendedSQL   string
	Detail           map[string]any
}
