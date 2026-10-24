package changereview

import (
	"time"

	"github.com/pg-sage/sidecar/internal/agentloop"
)

// Review is one stored review, mirroring the sage.change_review columns.
// Timestamps are UTC with microsecond precision. Fields that are NULL in
// the table are zero values here ("" for text, nil for pointers).
type Review struct {
	ID       string `json:"id"`
	Database string `json:"database"`
	Source   string `json:"source"`
	Actor    string `json:"actor"`

	Repo          string     `json:"repo,omitempty"`
	PRURL         string     `json:"pr_url,omitempty"`
	PRNumber      int        `json:"pr_number,omitempty"`
	HeadSHA       string     `json:"head_sha,omitempty"`
	BaseSHA       string     `json:"base_sha,omitempty"`
	PRTitle       string     `json:"pr_title,omitempty"`
	PROpenedAt    *time.Time `json:"pr_opened_at,omitempty"`
	MigrationTool string     `json:"migration_tool,omitempty"`
	Transactional *bool      `json:"transactional,omitempty"`

	RevisionOf  string `json:"revision_of,omitempty"`
	InputHash   string `json:"input_hash"`
	ContentHash string `json:"content_hash,omitempty"`
	DiffSHA256  string `json:"diff_sha256,omitempty"`

	State         string `json:"state"`
	FailureReason string `json:"failure_reason,omitempty"`
	Stage         string `json:"stage,omitempty"`

	Verdict              string `json:"verdict,omitempty"`
	DeterministicVerdict string `json:"deterministic_verdict,omitempty"`
	ParseMode            string `json:"parse_mode,omitempty"`
	ModelStatus          string `json:"model_status,omitempty"`

	Statements    []Statement           `json:"statements"`
	Findings      []Finding             `json:"findings"`
	Evidence      []Evidence            `json:"evidence"`
	Dropped       []Dropped             `json:"dropped"`
	NotDetermined []NotDetermined       `json:"not_determined"`
	Summary       ReviewSummary         `json:"summary"`
	Expectations  Expectations          `json:"expectations"`
	Transcript    *agentloop.Transcript `json:"transcript,omitempty"`
	Markdown      string                `json:"markdown,omitempty"`
	Tokens        int                   `json:"tokens"`
	ReceiptHash   string                `json:"receipt_hash,omitempty"`

	DBAVerdict         string     `json:"dba_verdict,omitempty"`
	DBADirection       string     `json:"dba_direction,omitempty"`
	DBAReason          string     `json:"dba_reason,omitempty"`
	DBAActor           string     `json:"dba_actor,omitempty"`
	DBADecidedAt       *time.Time `json:"dba_decided_at,omitempty"`
	VerdictCarriedFrom string     `json:"verdict_carried_from,omitempty"`

	LandingState  string     `json:"landing_state,omitempty"`
	LandingSource string     `json:"landing_source,omitempty"`
	LandedLower   *time.Time `json:"landed_lower,omitempty"`
	LandedAt      *time.Time `json:"landed_at,omitempty"`

	ReceiptVerdict   string     `json:"receipt_verdict,omitempty"`
	Receipt          *Receipt   `json:"receipt,omitempty"`
	ReceiptDecidedAt *time.Time `json:"receipt_decided_at,omitempty"`

	ShadowAutoClear *bool  `json:"shadow_auto_clear,omitempty"`
	ShadowScore     string `json:"shadow_score"`

	CreditState      string     `json:"credit_state"`
	CreditedMinutes  int        `json:"credited_minutes"`
	ToilModelVersion *int       `json:"toil_model_version,omitempty"`
	CreditFinalAt    *time.Time `json:"credit_final_at,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Credit is the value-ledger part of a review (credit.go).
func (r Review) Credit() Credit {
	c := Credit{State: r.CreditState, Minutes: r.CreditedMinutes, FinalAt: r.CreditFinalAt}
	if r.ToilModelVersion != nil {
		c.ToilModelVersion = *r.ToilModelVersion
	}
	return c
}

// DBAVerdict is a DBA's decision on a completed review. Override needs a
// Reason and a Direction; Reason is at most MaxReasonRunes.
type DBAVerdict struct {
	Verdict   string `json:"verdict"`
	Direction string `json:"direction,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Actor     string `json:"actor"`
}

// Landing is when a completed review's change landed: between Lower and
// At (Lower <= At), detected by Source.
type Landing struct {
	Source string    `json:"source"`
	Lower  time.Time `json:"lower"`
	At     time.Time `json:"at"`
}

// Deployment is a report that a reviewed change was deployed: by review
// ID, or by PRURL (the newest completed review of that PR). DeployedAt
// may not be in the future.
type Deployment struct {
	ID         string    `json:"id,omitempty"`
	PRURL      string    `json:"pr_url,omitempty"`
	DeployedAt time.Time `json:"deployed_at"`
	Commit     string    `json:"commit,omitempty"`
}

// Receipt is the post-deploy verdict of a landed review (decided once;
// the receipt workstream fills it).
type Receipt struct {
	Verdict      string          `json:"verdict"`
	Reason       string          `json:"reason,omitempty"`
	WindowHours  int             `json:"window_hours"`
	BeforeFrom   time.Time       `json:"before_from"`
	BeforeTo     time.Time       `json:"before_to"`
	AfterFrom    time.Time       `json:"after_from"`
	AfterTo      time.Time       `json:"after_to"`
	Targets      []ReceiptTarget `json:"targets"`
	Tolerance    string          `json:"tolerance,omitempty"`
	Incidents    []string        `json:"incidents,omitempty"`
	LockFindings []int64         `json:"lock_findings,omitempty"`
	BornQueryIDs []BornQuery     `json:"born_queryids,omitempty"`
	Confounders  []string        `json:"confounders,omitempty"`
	// ContradictedBy names what contradicted the review ("queryid:123",
	// "incident:<uuid>", "finding:42"), so a postmortem can cite it.
	ContradictedBy []string `json:"contradicted_by,omitempty"`
	EvidenceIDs    []string `json:"evidence_ids,omitempty"`
}

// ReceiptTarget is one watched queryid's before/after comparison.
type ReceiptTarget struct {
	QueryID      int64   `json:"queryid"`
	Verdict      string  `json:"verdict"`
	BeforeMeanMs float64 `json:"before_mean_ms"`
	AfterMeanMs  float64 `json:"after_mean_ms"`
	BeforeCalls  int64   `json:"before_calls"`
	AfterCalls   int64   `json:"after_calls"`
	ChangePct    float64 `json:"change_pct"`
	PValue       float64 `json:"p_value,omitempty"`
}

// BornQuery is a queryid first seen after landing on a touched relation.
type BornQuery struct {
	QueryID   int64     `json:"queryid"`
	FirstSeen time.Time `json:"first_seen"`
	MeanMs    float64   `json:"mean_ms"`
	Calls     int64     `json:"calls"`
	Relations []string  `json:"relations,omitempty"`
}

// ListFilter selects reviews for List, newest first. Limit is
// 1..MaxListLimit (DefaultListLimit when 0).
type ListFilter struct {
	PRURL string `json:"pr_url,omitempty"`
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
