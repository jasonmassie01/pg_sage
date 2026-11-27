package cases

import "time"

type SourceType string

const (
	SourceFindingType  SourceType = "finding"
	SourceIncidentType SourceType = "incident"
	SourceQueryType    SourceType = "query_hint"
	SourceForecastType SourceType = "forecast"
	SourceSchemaType   SourceType = "schema_health"
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
	// QueryID is the evidence's verified pg_stat_statements id as a decimal
	// string, so a browser never rounds it (tuning-10: the Explain panel).
	QueryID string `json:"queryid,omitempty"`
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
	// ActionsUnavailable is set when the action history could not be read,
	// so an empty Actions does not read as "nothing was done" (cases-05).
	ActionsUnavailable bool `json:"actions_unavailable,omitempty"`
	// Urgency is the time to impact the producer measured, with its basis
	// (cases-07); unassessed when it measured none.
	Urgency Urgency `json:"urgency"`
	// RankBasis says why the case ranks where it does in the global list.
	RankBasis string `json:"rank_basis,omitempty"`
	// Recurrence links a recurring incident to its predecessor; nil for a
	// first occurrence and for every non-incident case.
	Recurrence *Recurrence `json:"recurrence,omitempty"`
	// Recommendation is the newest recommendation head of the case's
	// finding (cases-10); nil when the finding has none.
	Recommendation *CaseRecommendation `json:"recommendation,omitempty"`
	// RecommendationUnknown is set when the heads could not be read, so a
	// nil Recommendation does not read as "never proposed".
	RecommendationUnknown bool `json:"recommendation_unknown,omitempty"`
	// ProviderAdvice is the provider's own advice on the case's finding
	// (market-09); ProviderAdviceUnavailable is set when it could not be read.
	ProviderAdvice            []ProviderAdvice `json:"provider_advice,omitempty"`
	ProviderAdviceUnavailable bool             `json:"provider_advice_unavailable,omitempty"`
	// Plans are the plans the case's finding is a step of (ux-03);
	// PlansUnavailable is set when they could not be read.
	Plans            []PlanLink `json:"plans,omitempty"`
	PlansUnavailable bool       `json:"plans_unavailable,omitempty"`
}

// Recurrence is an incident's place in its chain of recurrences
// (sage.incidents.previous_incident_id). Count includes this incident and
// is 0 when the history could not be read (HistoryUnavailable).
type Recurrence struct {
	PreviousID         string     `json:"previous_id"`
	Count              int        `json:"count,omitempty"`
	PreviousResolvedAt *time.Time `json:"previous_resolved_at,omitempty"`
	HistoryUnavailable bool       `json:"history_unavailable,omitempty"`
}

type ActionCandidate struct {
	ActionType                string                `json:"action_type"`
	RiskTier                  string                `json:"risk_tier"`
	Confidence                float64               `json:"confidence"`
	ProposedSQL               string                `json:"proposed_sql,omitempty"`
	ExpiresAt                 *time.Time            `json:"expires_at,omitempty"`
	BlockedReason             string                `json:"blocked_reason,omitempty"`
	OutputModes               []string              `json:"output_modes,omitempty"`
	RollbackClass             string                `json:"rollback_class,omitempty"`
	RecoveryDetail            string                `json:"recovery_detail,omitempty"`
	VerificationPlan          []string              `json:"verification_plan,omitempty"`
	DDLPreflight              *DDLPreflightReport   `json:"ddl_preflight,omitempty"`
	ScriptOutput              *ScriptOutput         `json:"script_output,omitempty"`
	BackendIdentity           *IncidentBlocker      `json:"backend_identity,omitempty"`
	Guardrails                []string              `json:"guardrails,omitempty"`
	PolicyDecision            *ActionPolicyDecision `json:"policy_decision,omitempty"`
	RequiresApproval          bool                  `json:"requires_approval,omitempty"`
	RequiresMaintenanceWindow bool                  `json:"requires_maintenance_window,omitempty"`
	// RetryingAfter is when a failed attempt of this content is retried,
	// while its recommendation is in backoff (cases-10).
	RetryingAfter *time.Time `json:"retrying_after,omitempty"`
	// PRNote is what PR mode did with the change (pr-01b): "opened as
	// <repo>#<n>", "declined in <repo>#<n>" or "PR not opened: <reason>".
	PRNote string `json:"pr_note,omitempty"`
	// Source is the finding the candidate was projected from, so its
	// policy verdict is explained as the executor would authorize that
	// finding (cases-04). Nil for incident and query-hint candidates.
	Source *SourceFinding `json:"-"`
}

type DDLPreflightReport struct {
	Summary         string  `json:"summary"`
	RiskScore       float64 `json:"risk_score,omitempty"`
	LockLevel       string  `json:"lock_level,omitempty"`
	RequiresRewrite bool    `json:"requires_rewrite"`
	TableName       string  `json:"table_name,omitempty"`
	TableSizeBytes  int64   `json:"table_size_bytes,omitempty"`
	EstimatedRows   int64   `json:"estimated_rows,omitempty"`
	ActiveQueries   int     `json:"active_queries,omitempty"`
	PendingLocks    int     `json:"pending_locks,omitempty"`
	ReplicationLag  float64 `json:"replication_lag_seconds,omitempty"`
	EstimatedLockMs int64   `json:"estimated_lock_ms,omitempty"`
	// MeasuredAt is when the source finding's live values were measured
	// (RFC 3339, at detection); empty when nothing was measured.
	MeasuredAt string           `json:"measured_at,omitempty"`
	Checks     []PreflightCheck `json:"checks,omitempty"`
}

type PreflightCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type ScriptOutput struct {
	Filename        string   `json:"filename"`
	MigrationSQL    string   `json:"migration_sql"`
	RollbackSQL     string   `json:"rollback_sql,omitempty"`
	VerificationSQL []string `json:"verification_sql,omitempty"`
	PRTitle         string   `json:"pr_title,omitempty"`
	PRBody          string   `json:"pr_body,omitempty"`
	RiskLabels      []string `json:"risk_labels,omitempty"`
	Format          string   `json:"format"`
}

func (a ActionCandidate) IsExecutable(now time.Time) bool {
	if a.ExpiresAt == nil {
		return true
	}
	return now.Before(*a.ExpiresAt)
}

type ActionPolicyDecision struct {
	Decision                  string   `json:"decision"`
	RiskTier                  string   `json:"risk_tier"`
	RequiresApproval          bool     `json:"requires_approval"`
	RequiresMaintenanceWindow bool     `json:"requires_maintenance_window"`
	BlockedReason             string   `json:"blocked_reason,omitempty"`
	Guardrails                []string `json:"guardrails,omitempty"`
	Provider                  string   `json:"provider,omitempty"`
}

type CaseAction struct {
	ID                 string     `json:"id,omitempty"`
	Type               string     `json:"type"`
	RiskTier           string     `json:"risk_tier"`
	Status             string     `json:"status"`
	PolicyDecision     string     `json:"policy_decision,omitempty"`
	LifecycleState     string     `json:"lifecycle_state,omitempty"`
	BlockedReason      string     `json:"blocked_reason,omitempty"`
	VerificationStatus string     `json:"verification_status,omitempty"`
	AttemptCount       int        `json:"attempt_count,omitempty"`
	ShadowToilMinutes  int        `json:"shadow_toil_minutes,omitempty"`
	Guardrails         []string   `json:"guardrails,omitempty"`
	ProposedAt         *time.Time `json:"proposed_at,omitempty"`
	ExpiresAt          time.Time  `json:"expires_at,omitempty"`
	CooldownUntil      *time.Time `json:"cooldown_until,omitempty"`
}

func NewCase(input CaseInput) Case {
	now := input.ObservedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}

	whyNow := input.WhyNow
	if whyNow == "" {
		whyNow = "Urgency not assessed"
	}
	normalizeRecoveryClaims(input.ActionCandidates)

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
	RollbackSQL      string
	Detail           map[string]any
	// ObservedAt is when the source finding was last observed
	// (sage.findings.last_seen). Zero means unknown.
	ObservedAt time.Time
}
