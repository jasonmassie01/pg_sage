package changereview

import (
	"context"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
)

// Input is what every stage sees of one review. Diff is the raw, untrusted
// unified diff: stages redact it before any model call and never store it.
// SQLFiles holds the request's inline SQL first (Path "") and then its
// files, in order. Transactional is resolved: the request's value, else
// the migration tool's default (TransactionalFrom says which).
type Input struct {
	ReviewID          string
	Database          string
	Actor             string
	SQLFiles          []SQLFile
	Diff              string
	PR                PRInfo
	MigrationTool     string
	Transactional     bool
	TransactionalFrom string
	Config            config.ChangeReviewConfig
}

// TransactionalFrom values.
const (
	TransactionalFromRequest = "request"
	TransactionalFromTool    = "tool_default"
)

// Analysis is the deterministic stage's result. Findings are rule
// findings (Origin OriginRule, severity info|warning|critical) citing
// Evidence; the service stores them byte-for-byte and derives the
// deterministic verdict from them with VerdictOf. ParseMode is ParseAST
// or ParseText.
type Analysis struct {
	ParseMode     string
	Statements    []Statement
	Findings      []Finding
	Evidence      []Evidence
	NotDetermined []NotDetermined
	Expectations  Expectations
}

// Reasoning is the model stage's result. Findings are clamped to at most
// warning by the merge and keep OriginModel (CR-20 may come as a rule
// finding when no model is available); Statements are the accepted
// translated or extracted statements; Dropped lists what the gates
// removed. Transcript is already redacted.
type Reasoning struct {
	ModelStatus   string
	Statements    []Statement
	Findings      []Finding
	Evidence      []Evidence
	Dropped       []Dropped
	NotDetermined []NotDetermined
	Intent        string
	Tokens        int
	Transcript    *agentloop.Transcript
}

// Inspection is one statement inspected by the impact engine: parsed and
// planned (plan-only), fingerprint-matched to pg_stat_statements, with the
// rule hits and evidence for that statement alone.
type Inspection struct {
	Statement  Statement
	Parsed     bool
	Planned    bool
	ParseError string
	PlanError  string
	Findings   []Finding
	Evidence   []Evidence
}

// Analyzer is the deterministic impact engine (internal/changereview/
// impact). It must finish within the context's deadline (20 s) and treat
// unknown as unknown (CR-17), never as none.
type Analyzer interface {
	Analyze(ctx context.Context, in Input) (Analysis, error)
}

// Reasoner is the one model run of a review (internal/changereview/
// reasoner). It reports provider trouble as Reasoning.ModelStatus; an
// error means the stage itself failed (the review keeps its deterministic
// result with model_status error).
type Reasoner interface {
	Reason(ctx context.Context, in Input, a Analysis) (Reasoning, error)
}

// Inspector inspects one statement (the reasoner's inspect_statement
// tool). origin is OriginSQLInput, OriginModelTranslated or
// OriginModelExtracted.
type Inspector interface {
	InspectStatement(ctx context.Context, sql, origin string) (Inspection, error)
}

// Notifier hears about reviews that need a DBA: a review that completed
// with verdict review or risky and no carried-over DBA verdict, once.
// Implementations must not block for long; errors are theirs to log.
type Notifier interface {
	ReviewNeedsDBA(ctx context.Context, r Review)
}
