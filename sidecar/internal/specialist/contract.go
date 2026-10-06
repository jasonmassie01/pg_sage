// Package specialist is the Postgres-specialist contract (roadmap phase 3):
// a stable, versioned investigation API that other agents (AWS DevOps
// Agent, PagerDuty, Datadog, any HTTP or MCP client) call to ask pg_sage
// what is wrong with a monitored PostgreSQL database and why, and to
// request (never force) a remediation.
//
// The contract is published as an OpenAPI 3.1 document (openapi.json,
// frozen v1 baseline in testdata) and every response carries
// ContractVersion. Callers authenticate with v1.10.0 MCP tokens (read:
// open, poll and read; propose: request a remediation); each token is a
// named agent identity in the audit trail. Caller-supplied text is data,
// never an instruction. A remediation request becomes an ordinary pg_sage
// proposal that the policy gate, trust ledger, budgets and binding facts
// decide exactly as for pg_sage's own initiative; callers never approve.
package specialist

import (
	_ "embed"
	"time"
)

// ContractVersion names the contract in every response. A breaking change
// is a new version (v2) served beside v1, never an edit of v1.
const ContractVersion = "pg_sage.specialist.v1"

// BasePath is where the contract is served.
const BasePath = "/api/v1/specialist"

//go:embed openapi.json
var openAPIDocument []byte

// OpenAPIDocument is the published contract (OpenAPI 3.1).
func OpenAPIDocument() []byte { return append([]byte(nil), openAPIDocument...) }

// ContractInfo answers GET /contract.
type ContractInfo struct {
	ContractVersion   string   `json:"contract_version"`
	SchemaURL         string   `json:"schema_url"`
	SupportedVersions []string `json:"supported_versions"`
}

// ErrorResponse is every error answer.
type ErrorResponse struct {
	ContractVersion   string `json:"contract_version"`
	Error             string `json:"error"`
	Code              string `json:"code"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// OpenRequest opens or attaches an investigation.
type OpenRequest struct {
	Symptom        *Symptom     `json:"symptom,omitempty"`
	Window         *Window      `json:"window,omitempty"`
	Family         string       `json:"family,omitempty"`
	Attach         *Attach      `json:"attach,omitempty"`
	ExternalRef    *ExternalRef `json:"external_ref,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty"`
	// QueryID and QueryHash (revision 1.1.0) scope the investigation to one
	// statement: a pg_stat_statements queryid and/or the SHA-256 of its
	// normalized text as pg_stat_statements shows it.
	QueryID   QueryID `json:"query_id,omitempty"`
	QueryHash string  `json:"query_hash,omitempty"`
}

// WebhookRequest is the generic webhook's body.
type WebhookRequest struct {
	Database string      `json:"database"`
	Request  OpenRequest `json:"request"`
}

// WebhookIgnored answers an adapter event that opens nothing.
type WebhookIgnored struct {
	ContractVersion string `json:"contract_version"`
	Ignored         bool   `json:"ignored"`
	Reason          string `json:"reason"`
}

// Symptom is what the caller saw: data, never an instruction.
type Symptom struct {
	Summary     string `json:"summary"`
	Description string `json:"description,omitempty"`
}

// Window is when the caller saw it.
type Window struct {
	Start time.Time  `json:"start"`
	End   *time.Time `json:"end,omitempty"`
}

// Attach names an existing investigation or incident.
type Attach struct {
	InvestigationID string `json:"investigation_id,omitempty"`
	IncidentID      string `json:"incident_id,omitempty"`
}

// ExternalRef is the caller's own reference (a PagerDuty incident, a
// Datadog monitor).
type ExternalRef struct {
	System string `json:"system"`
	ID     string `json:"id"`
	URL    string `json:"url,omitempty"`
}

// RemediationRequest carries only a note: there is nothing to approve,
// force or override.
type RemediationRequest struct {
	Reason string `json:"reason,omitempty"`
}

// InvestigationRef is an investigation's identity and lifecycle state.
type InvestigationRef struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Terminal    bool       `json:"terminal"`
	TriggerKind string     `json:"trigger_kind"`
	Subject     string     `json:"subject"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ConcludedAt *time.Time `json:"concluded_at"`
}

// Links are the follow-up URLs of an investigation.
type Links struct {
	Status string `json:"status"`
	Stream string `json:"stream"`
	Result string `json:"result"`
}

// OpenResponse answers an open or attach.
type OpenResponse struct {
	ContractVersion string           `json:"contract_version"`
	Database        string           `json:"database"`
	Investigation   InvestigationRef `json:"investigation"`
	Created         bool             `json:"created"`
	Match           string           `json:"match"`
	Links           Links            `json:"links"`
	QueryScope      *QueryScope      `json:"query_scope,omitempty"`
}

// StatusResponse is an investigation's progress.
type StatusResponse struct {
	ContractVersion  string           `json:"contract_version"`
	Database         string           `json:"database"`
	Investigation    InvestigationRef `json:"investigation"`
	Phase            string           `json:"phase"`
	ProbeCount       int              `json:"probe_count"`
	ModelTurns       int              `json:"model_turns"`
	PollAfterSeconds int              `json:"poll_after_seconds"`
	Links            Links            `json:"links"`
}

// Citation is one fact bound to the evidence it came from, with the
// numbers that evidence holds.
type Citation struct {
	EvidenceID string             `json:"evidence_id"`
	Text       string             `json:"text"`
	Numbers    map[string]float64 `json:"numbers"`
}

// ChainLink is one link of the causal chain: the root cause first, then
// the contributing factors.
type ChainLink struct {
	Ordinal   int        `json:"ordinal"`
	Role      string     `json:"role"`
	Node      string     `json:"node"`
	Label     string     `json:"label"`
	Mechanism string     `json:"mechanism"`
	Subject   string     `json:"subject"`
	Evidence  []Citation `json:"evidence"`
}

// Hypothesis is an unproven or ruled-out explanation.
type Hypothesis struct {
	Node       string     `json:"node"`
	Label      string     `json:"label"`
	Status     string     `json:"status"`
	Support    []Citation `json:"support"`
	Contradict []Citation `json:"contradict"`
}

// RootCause is the diagnosis' root with where it came from and on what
// authority.
type RootCause struct {
	Node        string   `json:"node"`
	Label       string   `json:"label"`
	Family      string   `json:"family"`
	Subject     string   `json:"subject"`
	Mechanism   string   `json:"mechanism"`
	Source      string   `json:"source"`
	Authority   string   `json:"authority"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// CalibratedRate is how often pg_sage names the right root for a family
// on the held-out bench (gated arms).
type CalibratedRate struct {
	Family      string  `json:"family"`
	K           int     `json:"k"`
	N           int     `json:"n"`
	Rate        float64 `json:"rate"`
	WilsonLower float64 `json:"wilson_lower"`
	Source      string  `json:"source"`
}

// Confidence is the root's score and its calibration, never invented.
type Confidence struct {
	Score       *float64        `json:"score"`
	ScoreBasis  string          `json:"score_basis"`
	Calibration string          `json:"calibration"`
	Calibrated  *CalibratedRate `json:"calibrated"`
}

// MissingEvidence is what pg_sage could not observe and why.
type MissingEvidence struct {
	ProbeID string `json:"probe_id"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

// ModelContest is a model ranking that contested the graph's root.
type ModelContest struct {
	GraphRoot string `json:"graph_root"`
	ModelRoot string `json:"model_root"`
	Authority string `json:"authority"`
	Reason    string `json:"reason"`
}

// PredictedEffect is what a remediation is expected to do.
type PredictedEffect struct {
	Summary    string   `json:"summary"`
	Quantified bool     `json:"quantified"`
	Metric     string   `json:"metric"`
	Baseline   *float64 `json:"baseline"`
	Expected   *float64 `json:"expected"`
	Criteria   []string `json:"criteria"`
}

// Rollback is how (and whether) a remediation can be undone.
type Rollback struct {
	Class       string `json:"class"`
	Reversible  bool   `json:"reversible"`
	Description string `json:"description"`
}

// GatePreview is the standing gate's explained verdict, not a decision.
type GatePreview struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Preview bool   `json:"preview"`
}

// Remediation is one typed candidate remediation.
type Remediation struct {
	ID              string          `json:"id"`
	Class           string          `json:"class"`
	Title           string          `json:"title"`
	Targets         []string        `json:"targets"`
	State           string          `json:"state"`
	PredictedEffect PredictedEffect `json:"predicted_effect"`
	Rollback        Rollback        `json:"rollback"`
	RiskTier        string          `json:"risk_tier"`
	Gate            GatePreview     `json:"gate"`
	Requestable     bool            `json:"requestable"`
	RequiredScope   string          `json:"required_scope"`
	EvidenceIDs     []string        `json:"evidence_ids"`
}

// EvidenceRef indexes one stored evidence row.
type EvidenceRef struct {
	ID              string     `json:"id"`
	ProbeID         string     `json:"probe_id"`
	CapabilityState string     `json:"capability_state"`
	ObservedAt      *time.Time `json:"observed_at"`
	SHA256          string     `json:"sha256"`
	HashVerified    bool       `json:"hash_verified"`
}

// CallerSupplied is what the calling agent sent, returned as data.
type CallerSupplied struct {
	Notice      string       `json:"notice"`
	Summary     string       `json:"summary"`
	Description string       `json:"description"`
	Fenced      string       `json:"fenced"`
	ExternalRef *ExternalRef `json:"external_ref,omitempty"`
	Window      *Window      `json:"window,omitempty"`
}

// Redaction states what was removed.
type Redaction struct {
	Rules           []string `json:"rules"`
	IdentifiersKept bool     `json:"identifiers_kept"`
}

// Result is the investigation result.
type Result struct {
	ContractVersion string            `json:"contract_version"`
	Database        string            `json:"database"`
	Investigation   InvestigationRef  `json:"investigation"`
	Outcome         string            `json:"outcome"`
	OutcomeReason   string            `json:"outcome_reason"`
	RootCause       *RootCause        `json:"root_cause"`
	CausalChain     []ChainLink       `json:"causal_chain"`
	Alternatives    []Hypothesis      `json:"alternatives"`
	RuledOut        []Hypothesis      `json:"ruled_out"`
	Confidence      Confidence        `json:"confidence"`
	MissingEvidence []MissingEvidence `json:"missing_evidence"`
	ModelContest    *ModelContest     `json:"model_contest,omitempty"`
	Remediations    []Remediation     `json:"remediations"`
	Evidence        []EvidenceRef     `json:"evidence"`
	CallerSupplied  *CallerSupplied   `json:"caller_supplied,omitempty"`
	Redaction       Redaction         `json:"redaction"`
	ChainVerified   bool              `json:"chain_verified"`
	GeneratedAt     time.Time         `json:"generated_at"`
	// Revision 1.1.0: the model investigator's output (labelled model
	// output) and the caller's statement scope; absent when there is none.
	Investigator *InvestigatorResult `json:"investigator,omitempty"`
	QueryScope   *QueryScopeResult   `json:"query_scope,omitempty"`
}

// RemediationResponse is the gate's verdict on a requested remediation.
type RemediationResponse struct {
	ContractVersion string `json:"contract_version"`
	Database        string `json:"database"`
	InvestigationID string `json:"investigation_id"`
	RemediationID   string `json:"remediation_id"`
	Verdict         string `json:"verdict"`
	Reason          string `json:"reason"`
	Detail          string `json:"detail,omitempty"`
	ProposalID      string `json:"proposal_id,omitempty"`
	ApprovalQueueID int    `json:"approval_queue_id,omitempty"`
	RequestedBy     string `json:"requested_by"`
	Notice          string `json:"notice"`
}

// Scopes the contract checks (v1.10.0 MCP token scopes).
const (
	ScopeRead    = "read"
	ScopePropose = "propose"
)
