package specialist

// Contract revision 1.1.0 (additive within pg_sage.specialist.v1): the
// tool-calling investigator's output and the caller's statement scope.
// Every field is optional in v1; a client that ignores unknown fields
// reads exactly the v1 result.

// InvestigatorLabel labels the investigator's section: model output.
const InvestigatorLabel = "model investigator conclusion"

// Investigator verdicts: the model's answer normalized against the causal
// graph (an answer the contract does not know is inconclusive).
const (
	VerdictAgree        = "agree"
	VerdictConclude     = "conclude"
	VerdictContest      = "contest"
	VerdictUnmodeled    = "unmodeled"
	VerdictInconclusive = "inconclusive"
	VerdictNoAnswer     = "no_answer"
)

// Root adoption states: a contested or concluded model root is adopted
// only under the family's earned root authority (model-lift on the
// held-out bench); otherwise it stays advisory (L1). Agreement and no
// conclusion adopt nothing.
const (
	AdoptionAdopted       = "adopted"
	AdoptionAdvisory      = "advisory"
	AdoptionNotApplicable = "not_applicable"
)

// Query scope application: a plan-regression investigation's probe and
// diagnosis are about the statement; any other family reads the whole
// database and the result names the evidence that mentions the statement.
const (
	QueryAppliedProbes   = "probes_and_diagnosis"
	QueryAppliedEvidence = "evidence"
)

// InvestigatorResult is what the model investigator concluded, on what
// authority, citing what.
type InvestigatorResult struct {
	Label           string              `json:"label"`
	Verdict         string              `json:"verdict"`
	Root            string              `json:"root"`
	GraphRoot       string              `json:"graph_root"`
	Cause           *UnmodeledCause     `json:"cause"`
	Adoption        RootAdoption        `json:"adoption"`
	Claims          []InvestigatorClaim `json:"claims"`
	DroppedClaims   int                 `json:"dropped_claims"`
	MissingEvidence []MissingEvidence   `json:"missing_evidence"`
	Run             InvestigatorRun     `json:"run"`
	Transcript      *TranscriptLink     `json:"transcript"`
}

// RootAdoption is whether the model's root was adopted, under which
// family's authority, and why.
type RootAdoption struct {
	Status string `json:"status"`
	Family string `json:"family"`
	Reason string `json:"reason"`
}

// InvestigatorClaim is one verified claim with the evidence it cites.
type InvestigatorClaim struct {
	Text     string     `json:"text"`
	Evidence []Citation `json:"evidence"`
}

// UnmodeledCause is a cause the causal graph has no node for.
type UnmodeledCause struct {
	Label     string `json:"label"`
	Mechanism string `json:"mechanism"`
}

// InvestigatorRun summarizes the investigator's run.
type InvestigatorRun struct {
	Plan       string `json:"plan"`
	Protocol   string `json:"protocol"`
	Stop       string `json:"stop"`
	ModelCalls int    `json:"model_calls"`
	ToolCalls  int    `json:"tool_calls"`
	Probes     int    `json:"probes"`
}

// TranscriptLink is where the redacted transcript is served.
type TranscriptLink struct {
	Href     string `json:"href"`
	MCPTool  string `json:"mcp_tool"`
	Schema   string `json:"schema"`
	Redacted bool   `json:"redacted"`
}

// TranscriptResponse is the redacted investigator transcript.
type TranscriptResponse struct {
	ContractVersion string         `json:"contract_version"`
	Database        string         `json:"database"`
	InvestigationID string         `json:"investigation_id"`
	Transcript      map[string]any `json:"transcript"`
}

// QueryScope is the statement a caller scoped its investigation to.
type QueryScope struct {
	QueryID   string `json:"query_id"`
	QueryHash string `json:"query_hash,omitempty"`
	Applied   string `json:"applied"`
}

// QueryScopeResult is the scope in a result: whether the root is about the
// statement and which evidence mentions it.
type QueryScopeResult struct {
	QueryID     string   `json:"query_id"`
	QueryHash   string   `json:"query_hash,omitempty"`
	Applied     string   `json:"applied"`
	RootMatches bool     `json:"root_matches"`
	EvidenceIDs []string `json:"evidence_ids"`
}
