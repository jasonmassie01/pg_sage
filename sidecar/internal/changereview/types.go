// Package changereview is Change Review: pg_sage, the agentic DBA, reviews
// a migration or data-access change against the live database before it
// merges, and stamps a receipt after it lands.
//
// The package is the foundation the stages build on: the request and
// review types, the stage interfaces (Analyzer, the deterministic impact
// engine; Reasoner, the one model run; Inspector, one statement), the
// Service with its bounded per-database runner and merge rule, the store
// (every transition a conditional UPDATE), the daily token budget, the
// hashes, the Markdown/JSON renderer, calibration, the credit state
// machine and the process-wide registry.
//
// Twenty deterministic rules (CR-01..CR-20) set the verdict floor. The
// model can raise safe to review, never reach risky, never lower a
// verdict and never edit a deterministic finding. Nothing here executes
// the reviewed SQL; the only writes are to sage.change_review and
// sage.change_review_budget_day. Diffs, SQL, file names, PR text and
// model output are untrusted data and never grant anything.
package changereview

import (
	"encoding/json"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Sources of a review request.
const (
	SourceMCP = "mcp"
	SourceAPI = "api"
	SourceCLI = "cli"
)

// Review states.
const (
	StateRunning  = "running"
	StateComplete = "complete"
	StateFailed   = "failed"
)

// Stages a running review is in (Review.Stage).
const (
	StageQueued        = "queued"
	StageDeterministic = "deterministic"
	StageModel         = "model"
	StageDone          = "done"
)

// FailureInterrupted is the failure reason of a review whose process
// stopped while it ran.
const FailureInterrupted = "interrupted"

// Verdicts, ordered safe < review < risky (VerdictRank).
const (
	VerdictSafe   = "safe"
	VerdictReview = "review"
	VerdictRisky  = "risky"
)

// Parse modes: ast (libpg_query) or text (the regex classifier, a nocgo
// build or SQL the parser rejects). Text mode never yields safe.
const (
	ParseAST  = "ast"
	ParseText = "text"
)

// Model statuses.
const (
	ModelOK          = "ok"
	ModelNoModel     = "no_model"
	ModelBudget      = "budget"
	ModelRateLimited = "rate_limited"
	ModelTimeout     = "timeout"
	ModelError       = "error"
	ModelSkipped     = "skipped"
)

// DBA verdicts and override directions.
const (
	DBAConcur          = "concur"
	DBAConcurWithEdits = "concur_with_edits"
	DBAOverride        = "override"

	DirectionTooStrict  = "too_strict"
	DirectionTooLenient = "too_lenient"
)

// Landing states and sources.
const (
	LandingAwaiting      = "awaiting"
	LandingLanded        = "landed"
	LandingExpired       = "expired"
	LandingNotApplicable = "not_applicable"

	LandingSourceCatalog     = "catalog"
	LandingSourceChangeEvent = "change_event"
	LandingSourceReported    = "reported"
)

// Receipt verdicts.
const (
	ReceiptPending         = "pending"
	ReceiptClean           = "clean"
	ReceiptCleanLowTraffic = "clean_low_traffic"
	ReceiptRegressed       = "regressed"
	ReceiptUnverifiable    = "unverifiable"
)

// Shadow auto-clear scores.
const (
	ShadowCorrect   = "correct"
	ShadowIncorrect = "incorrect"
	ShadowUnscored  = "unscored"
)

// Credit states (credit.go).
const (
	CreditNone    = "none"
	CreditPending = "pending"
	CreditFinal   = "final"
	CreditVoid    = "void"
)

// Finding severities and origins.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"

	OriginRule  = "rule"
	OriginModel = "model"
)

// Statement origins.
const (
	OriginSQLInput        = "sql_input"
	OriginModelTranslated = "model_translated"
	OriginModelExtracted  = "model_extracted"
)

// Rule ids the foundation itself raises or recognizes.
const (
	RuleTextMode         = "CR-18"
	RuleModelTranslated  = "CR-19"
	RuleNotAnalyzableSQL = "CR-20"
)

// Limits.
const (
	MaxWaitSeconds    = 50
	MaxSQLFiles       = 200
	MaxPathBytes      = 512
	MaxPRTitleRunes   = 300
	MaxReasonRunes    = 2000
	MaxWatchQueryIDs  = 20
	MaxCommentRunes   = 65000
	DedupeWindow      = 30 * 24 * time.Hour
	InterruptAfter    = 5 * time.Minute
	QueueLength       = 8
	MaxListLimit      = 100
	DefaultListLimit  = 20
	MaxActorRunes     = 200
	maxEvidenceIDLen  = 200
	maxSuggestionCode = 2048
)

// Caller is who asks: Actor is the stable identity ("user:42",
// "mcp:token:<id>"), Source the surface (SourceMCP, SourceAPI, SourceCLI).
// A caller can request reviews and report deploys; DBA verdicts are a
// separate call (Service.Decide) the surfaces allow only to operator and
// admin sessions.
type Caller struct {
	Actor  string `json:"actor"`
	Source string `json:"source"`
}

// SQLFile is one migration file of a request.
type SQLFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// PRInfo is the pull request a change belongs to; every field optional.
// URL must be https; HeadSHA and BaseSHA are 7..64 lower-case hex.
type PRInfo struct {
	URL      string     `json:"url,omitempty"`
	Number   int        `json:"number,omitempty"`
	Title    string     `json:"title,omitempty"`
	Repo     string     `json:"repo,omitempty"`
	HeadSHA  string     `json:"head_sha,omitempty"`
	BaseSHA  string     `json:"base_sha,omitempty"`
	OpenedAt *time.Time `json:"opened_at,omitempty"`
}

// Request is one review request: migration SQL (inline and/or files)
// and/or a unified diff, with optional PR metadata. MigrationTool is one
// of MigrationTools ("" for plain SQL); Transactional overrides the
// tool's default. WaitSeconds (0..MaxWaitSeconds) makes Submit wait for
// the review to complete.
type Request struct {
	Database      string    `json:"database,omitempty"`
	SQL           string    `json:"sql,omitempty"`
	SQLFiles      []SQLFile `json:"sql_files,omitempty"`
	Diff          string    `json:"diff,omitempty"`
	PR            PRInfo    `json:"pr"`
	MigrationTool string    `json:"migration_tool,omitempty"`
	Transactional *bool     `json:"transactional,omitempty"`
	WaitSeconds   int       `json:"wait_seconds,omitempty"`
}

// Statement is one SQL statement the review analyzed: from the request
// (OriginSQLInput, File and Line of the migration file) or derived by the
// model (OriginModelTranslated from ORM migration code, OriginModelExtracted
// from data-access code; File and Line point into the diff).
type Statement struct {
	Index          int      `json:"index"`
	Origin         string   `json:"origin"`
	File           string   `json:"file,omitempty"`
	Line           int      `json:"line,omitempty"`
	SQL            string   `json:"sql"`
	Kind           string   `json:"kind"`
	Relations      []string `json:"relations,omitempty"`
	Fingerprint    string   `json:"fingerprint,omitempty"`
	MatchedQueryID *int64   `json:"matched_queryid,omitempty"`
	PlanSummary    string   `json:"plan_summary,omitempty"`
	Confidence     float64  `json:"confidence,omitempty"`
	EvidenceID     string   `json:"evidence_id,omitempty"`
	ORM            string   `json:"orm,omitempty"`
	InLoop         bool     `json:"in_loop,omitempty"`
}

// Suggestion is a fix in the codebase's own idiom (model findings only);
// Code is at most 2 KB and always rendered as "suggested by pg_sage's
// model — verify".
type Suggestion struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

// Finding is one result: a rule hit (OriginRule, RuleID "CR-01".."CR-20")
// or a model finding (OriginModel, severity at most warning). Kind is the
// rule's name ("column_still_referenced") or the model's kind.
// EvidenceIDs cite Evidence of the same review.
type Finding struct {
	RuleID      string      `json:"rule_id,omitempty"`
	Kind        string      `json:"kind"`
	Severity    string      `json:"severity"`
	Origin      string      `json:"origin"`
	Title       string      `json:"title"`
	Text        string      `json:"text,omitempty"`
	EvidenceIDs []string    `json:"evidence_ids"`
	Suggestion  *Suggestion `json:"suggestion,omitempty"`
}

// Evidence is one citable fact: ID "kind:ref" ("stmt:1", "queryid:-42",
// "table:rv.rv_orders"), a short Label, the Data it rests on (canonical
// JSON) and Digest, the sha256 of ID, Label and Data (NewEvidence).
type Evidence struct {
	ID     string          `json:"id"`
	Kind   string          `json:"kind"`
	Label  string          `json:"label"`
	Digest string          `json:"digest"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// NotDetermined is something the review could not decide, and why
// ("app-code changes", "model not configured").
type NotDetermined struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

// Dropped is a model claim, finding or statement the gates removed:
// Kind "claim", "finding" or "statement"; Reason e.g. "uncited",
// "ungrounded", "not_parsed", "line_not_added", "unknown_evidence".
type Dropped struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// Post-condition kinds: what the catalog shows once a DDL statement has
// landed.
const (
	PostIndexValid       = "index_valid"
	PostIndexAbsent      = "index_absent"
	PostColumnPresent    = "column_present"
	PostColumnAbsent     = "column_absent"
	PostColumnRenamed    = "column_renamed"
	PostColumnType       = "column_type"
	PostNotNull          = "not_null"
	PostNullable         = "nullable"
	PostConstraint       = "constraint_present"
	PostConstraintAbsent = "constraint_absent"
	PostRelationPresent  = "relation_present"
	PostRelationAbsent   = "relation_absent"
	PostRelationRenamed  = "relation_renamed"
)

// PostCondition is one catalog check for one DDL statement. Relation is
// "schema.name"; the other fields are set as the kind needs (Column and
// NewName for a rename, Type for column_type, Index or Constraint names).
type PostCondition struct {
	StatementIndex int    `json:"statement_index"`
	Kind           string `json:"kind"`
	Relation       string `json:"relation"`
	Column         string `json:"column,omitempty"`
	NewName        string `json:"new_name,omitempty"`
	Type           string `json:"type,omitempty"`
	Index          string `json:"index,omitempty"`
	Constraint     string `json:"constraint,omitempty"`
}

// Expectations are what the receipt checks after the change lands: one
// post-condition per DDL statement, a watch set of at most
// MaxWatchQueryIDs queryids, the touched relations and a prediction per
// target (0, no change, unless HypoPG predicted one).
type Expectations struct {
	PostConditions []PostCondition     `json:"post_conditions"`
	WatchQueryIDs  []int64             `json:"watch_queryids"`
	Relations      []string            `json:"relations"`
	Predictions    []verify.Prediction `json:"predictions"`
}

// ReviewSummary is the per-review digest stored in the summary column.
type ReviewSummary struct {
	Critical        int          `json:"critical"`
	Warning         int          `json:"warning"`
	Info            int          `json:"info"`
	Rules           []string     `json:"rules"`
	ModelFindings   int          `json:"model_findings"`
	DroppedCount    int          `json:"dropped_count"`
	AppCodeAnalyzed bool         `json:"app_code_analyzed"`
	Intent          string       `json:"intent,omitempty"`
	Calibration     *Calibration `json:"calibration,omitempty"`
}
