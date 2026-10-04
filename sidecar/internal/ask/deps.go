package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/config"
)

// InvestigationSource reads the database's investigations (redacted).
// Investigation returns ErrNotFound (wrapped) for an unknown id.
type InvestigationSource interface {
	ListInvestigations(ctx context.Context, limit int) (json.RawMessage, error)
	Investigation(ctx context.Context, id string) (json.RawMessage, error)
}

// TrustSource reads the database's trust ledger view.
type TrustSource interface {
	Trust(ctx context.Context) (json.RawMessage, error)
}

// QuerySource reads the database's heaviest statements.
type QuerySource interface {
	TopQueries(ctx context.Context, r agenttools.TopQueriesRequest) (
		agenttools.TopQueriesResult, error)
}

// Proposal is a finding queued (or already waiting) for a person's
// approval. Prediction is the predicted effect as JSON.
type Proposal struct {
	QueueID       int64
	FindingID     int64
	Created       bool
	Verdict       string
	Reason        string
	RiskTier      string
	ActionType    string
	SQL           string
	RollbackSQL   string
	RollbackClass string
	Prediction    json.RawMessage
}

// Proposer queues one of pg_sage's own findings for a person's approval
// through the policy gate; it never executes. Errors wrap ErrRefused (not
// proposable) or ErrBlocked (the gate blocks it).
type Proposer interface {
	ProposeFinding(ctx context.Context, findingID int64, actor string) (Proposal, error)
}

// StartRequest opens an investigation on an operator's behalf.
type StartRequest struct {
	Subject string
	CaseID  string
	Actor   string
}

// Started is the investigation opened (or joined when one is running).
type Started struct {
	ID      string
	Created bool
}

// InvestigationStarter opens an investigation through the operator
// trigger. Errors wrap ErrRefused for a request the investigator refuses.
type InvestigationStarter interface {
	StartInvestigation(ctx context.Context, r StartRequest) (Started, error)
}

// Deps is one database's Ask Sage. Pool is the monitored database (its
// sage schema holds the records Ask Sage reads and its conversations and
// budget). Model nil means no LLM is configured. The sources and write
// paths are optional: a tool whose source is nil is not offered.
type Deps struct {
	Database       string
	Pool           *pgxpool.Pool
	Model          agentloop.Model
	Config         config.AskConfig
	Settings       *config.Config
	Investigations InvestigationSource
	Trust          TrustSource
	Queries        QuerySource
	Proposer       Proposer
	Starter        InvestigationStarter
	// Budget overrides the loop's per-question bounds (zero: DefaultBudget
	// with MaxTokens from Config.MaxTokensPerQuestion).
	Budget   agentloop.Budget
	Protocol agentloop.Protocol
	Now      func() time.Time
	Log      func(level, format string, args ...any)
}

// Service is one database's Ask Sage.
type Service struct {
	d      Deps
	store  *store
	budget *Budget
}

// DefaultBudget bounds one question: six model calls within 25 s (inside
// the API's 30 s request deadline), ten tool calls, 1500 completion
// tokens a call.
func DefaultBudget() agentloop.Budget {
	return agentloop.Budget{MaxSteps: 6, MaxCalls: 10, MaxCost: 10, Wall: 25 * time.Second,
		MaxTokens: config.DefaultAskMaxTokensPerQuestion, StepTokens: 1500,
		StepTimeout: 15 * time.Second}
}

// New builds a database's Ask Sage.
func New(d Deps) (*Service, error) {
	if strings.TrimSpace(d.Database) == "" || d.Pool == nil {
		return nil, fmt.Errorf("%w: a database name and pool are required", ErrInvalid)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Log == nil {
		d.Log = func(string, string, ...any) {}
	}
	if d.Settings == nil {
		d.Settings = config.DefaultConfig()
	}
	if d.Budget.MaxSteps == 0 {
		d.Budget = DefaultBudget()
		if d.Config.MaxTokensPerQuestion > 0 {
			d.Budget.MaxTokens = d.Config.MaxTokensPerQuestion
		}
	}
	if d.Protocol == "" {
		d.Protocol = agentloop.ProtocolAuto
	}
	return &Service{d: d, store: newStore(d.Pool), budget: NewBudget(d.Pool,
		int64(d.Config.DailyTokensPerDatabase), int64(d.Config.DailyTokensPerUser),
		d.Now)}, nil
}

// Database is the database the service answers about.
func (s *Service) Database() string { return s.d.Database }
