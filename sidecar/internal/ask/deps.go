package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
// proposable) or ErrBlocked (the gate blocks it). actor is the asking
// caller, recorded as the item's proposed_by with proposed_via ask_sage.
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
	// with MaxTokens from Config.MaxTokensPerQuestion and Wall from
	// Config.MaxSecondsPerQuestion).
	Budget   agentloop.Budget
	Protocol agentloop.Protocol
	Now      func() time.Time
	Log      func(level, format string, args ...any)
	// RunCtx is the context asynchronous runs execute on: the database
	// runtime's, so a run outlives its request and ends with the sidecar
	// (nil: context.Background()).
	RunCtx context.Context
	// Instance names this sidecar on its runs (default: the host name);
	// at start a sidecar recovers only its own abandoned runs.
	Instance string
}

// Service is one database's Ask Sage.
type Service struct {
	d      Deps
	store  *store
	budget *Budget
	runs   *runTracker
}

// DefaultBudget bounds one question: six model calls of at most 15 s each
// within ask.max_seconds_per_question's default (the API gives the ask
// route that plus 5 s), ten tool calls, 1500 completion tokens a call.
func DefaultBudget() agentloop.Budget {
	return agentloop.Budget{MaxSteps: 6, MaxCalls: 10, MaxCost: 10,
		Wall:      time.Duration(config.DefaultAskMaxSecondsPerQuestion) * time.Second,
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
		if d.Config.MaxSecondsPerQuestion > 0 {
			d.Budget.Wall = time.Duration(d.Config.MaxSecondsPerQuestion) * time.Second
		}
	}
	if d.Protocol == "" {
		d.Protocol = agentloop.ProtocolAuto
	}
	runDefaults(&d)
	return &Service{d: d, store: newStore(d.Pool), budget: NewBudget(d.Pool,
		int64(d.Config.DailyTokensPerDatabase), int64(d.Config.DailyTokensPerUser),
		d.Now), runs: newRunTracker(d.Config.MaxConcurrentRuns)}, nil
}

// runDefaults fills the asynchronous-run settings left zero.
func runDefaults(d *Deps) {
	if d.RunCtx == nil {
		d.RunCtx = context.Background()
	}
	if d.Config.MaxConcurrentRuns <= 0 {
		d.Config.MaxConcurrentRuns = config.DefaultAskMaxConcurrentRuns
	}
	if strings.TrimSpace(d.Instance) == "" {
		d.Instance = hostInstance()
	}
}

// hostInstance is this sidecar's instance name: the host name, which a
// restarted sidecar keeps.
func hostInstance() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "sidecar"
	}
	return clip(host, 200)
}

// Database is the database the service answers about.
func (s *Service) Database() string { return s.d.Database }
