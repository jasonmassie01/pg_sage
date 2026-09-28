package rca

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Incident represents a correlated root cause analysis result.
type Incident struct {
	ID              string      `json:"id"`
	DetectedAt      time.Time   `json:"detected_at"`
	LastDetectedAt  time.Time   `json:"last_detected_at"`
	Severity        string      `json:"severity"`
	RootCause       string      `json:"root_cause"`
	CausalChain     []ChainLink `json:"causal_chain"`
	AffectedObjects []string    `json:"affected_objects"`
	SignalIDs       []string    `json:"signal_ids"`
	RecommendedSQL  string      `json:"recommended_sql,omitempty"`
	RollbackSQL     string      `json:"rollback_sql,omitempty"`
	ActionRisk      string      `json:"action_risk,omitempty"`
	Source          string      `json:"source"`
	Confidence      float64     `json:"confidence"`
	ResolvedAt      *time.Time  `json:"resolved_at,omitempty"`
	DatabaseName    string      `json:"database_name,omitempty"`
	OccurrenceCount int         `json:"occurrence_count"`
	EscalatedAt     *time.Time  `json:"escalated_at,omitempty"`
	// ResolvedBy and ResolutionReason record who resolved the incident
	// and why (an operator or the engine itself).
	ResolvedBy       string `json:"resolved_by,omitempty"`
	ResolutionReason string `json:"resolution_reason,omitempty"`
	// PreviousIncidentID links a recurrence to the resolved incident with
	// the same identity that preceded it.
	PreviousIncidentID string `json:"previous_incident_id,omitempty"`
	// liveEvidence marks detections whose root cause and chain describe
	// the current state (lock chains): merging one into an open incident
	// refreshes that incident's evidence instead of keeping the first.
	liveEvidence bool
}

// ChainLink is one step in the causal chain leading to an incident.
type ChainLink struct {
	Order       int    `json:"order"`
	Signal      string `json:"signal"`
	Description string `json:"description"`
	Evidence    string `json:"evidence"`
	// Blocker is the structured backend identity behind a lock-chain
	// link (pid + backend_start + query identity).
	Blocker *BlockerIdentity `json:"blocker,omitempty"`
}

// Signal is a single fired detector result.
type Signal struct {
	ID       string         `json:"id"`
	FiredAt  time.Time      `json:"fired_at"`
	Severity string         `json:"severity"`
	Metrics  map[string]any `json:"metrics"`
	// blockers carries lock-chain backend identities to the incident
	// builder without adding them to Metrics (Tier 2 prompts).
	blockers []BlockerIdentity
}

// LogSource produces log-based RCA signals from the logwatch package.
type LogSource interface {
	Start(ctx context.Context) error
	Drain() []*Signal
	Stop()
}

// ActionQuerier reads recent pg_sage actions and rollback history
// from sage.action_log. Implemented by store.ActionStore.
type ActionQuerier interface {
	RecentSageActions(
		ctx context.Context, lookback time.Duration,
	) ([]SageAction, error)
	RollbackHistory(
		ctx context.Context, lookback time.Duration,
	) ([]SageAction, error)
}

// EventDispatcher delivers incident notifications. Satisfied by
// *notify.Dispatcher.
type EventDispatcher interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

// Engine is the root cause analysis engine supporting Tier 1
// (deterministic decision trees) and Tier 2 (LLM correlation).
//
// The engine is the single owner of incident state: it hydrates open
// incidents from sage.incidents, never overwrites a resolution recorded
// in the database, and drops resolved incidents from memory once their
// resolution is durable.
type Engine struct {
	cfg             *config.RCAConfig
	incidents       []Incident
	clearCounts     map[string]int         // incidentID -> clear cycles
	track           map[string]*trackState // incidentID -> persistence
	cycleCount      int
	gracePeriodLeft int
	capWarned       bool
	logFn           func(string, string, ...any)
	llmClient       *llm.Client
	logSource       LogSource
	actionStore     ActionQuerier
	correlator      *SelfActionCorrelator
	dispatcher      EventDispatcher
	databaseName    string
	logDatabase     string
	logReplayCutoff time.Time
	store           *pgxpool.Pool
	hydrated        bool
	// fastFired records signals the lock-chain fast path observed since
	// the last analyzer cycle; that cycle treats them as still firing.
	fastFired map[string]bool
	// cycleMu serializes analysis, hydration and persistence cycles.
	// mu guards the fields above and is never held across I/O.
	cycleMu sync.Mutex
	mu      sync.Mutex
}

// defaultLogReplayGrace tolerates clock skew between the PostgreSQL
// server (log timestamps) and the sidecar when ignoring log lines that
// were written before the engine started (G1-B25).
const defaultLogReplayGrace = 5 * time.Minute

// WithLLM attaches an LLM client to enable Tier 2 correlation.
// Safe to call on a nil client — Tier 2 simply stays disabled.
// The setter holds e.mu because wiring may happen after the
// analyzer goroutine has already started calling Analyze.
func (e *Engine) WithLLM(client *llm.Client) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.llmClient = client
}

// SetLogSource attaches a log-based signal source (logwatch adapter).
// When set, Analyze drains log signals each cycle.
func (e *Engine) SetLogSource(src LogSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logSource = src
}

// WithActionStore enables self-action correlation by wiring in a
// store that can query sage.action_log for recent actions and
// rollback history. The store must be scoped to this engine's
// database: actions it returns are attributed to that database.
func (e *Engine) WithActionStore(store ActionQuerier) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.actionStore = store
	e.correlator = NewSelfActionCorrelator(e.logFn)
}

// WithDispatcher enables incident_detected / incident_escalated /
// incident_resolved notifications. Events are sent after the state
// change is durable.
func (e *Engine) WithDispatcher(d EventDispatcher) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dispatcher = d
}

// WithDatabaseName sets the database identity stamped on every incident
// this engine constructs (R05). Use the same name the executor and
// notifications use for this database.
func (e *Engine) WithDatabaseName(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.databaseName = name
}

// WithLogDatabase restricts log signals to lines from the given
// PostgreSQL database (current_database()). Lines without a database
// (cluster-wide events) are always kept. Hydrate fills this from
// current_database() when it is unset (G1-B11).
func (e *Engine) WithLogDatabase(datname string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logDatabase = datname
}

// WithLogReplayCutoff ignores log signals timestamped before t. NewEngine
// defaults it to engine start minus defaultLogReplayGrace so a restart
// does not re-fire incidents from the replayed log tail (G1-B25). A zero
// time disables the cutoff.
func (e *Engine) WithLogReplayCutoff(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logReplayCutoff = t
}

// NewEngine creates a new RCA engine with the given configuration.
func NewEngine(
	cfg *config.RCAConfig,
	logFn func(string, string, ...any),
) *Engine {
	return &Engine{
		cfg:             cfg,
		incidents:       make([]Incident, 0),
		clearCounts:     make(map[string]int),
		track:           make(map[string]*trackState),
		fastFired:       make(map[string]bool),
		gracePeriodLeft: cfg.ResolutionCycles + 1,
		logFn:           logFn,
		logReplayCutoff: time.Now().Add(-defaultLogReplayGrace),
	}
}

// Analyze runs one RCA cycle without a caller context. Prefer
// AnalyzeContext.
func (e *Engine) Analyze(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) []Incident {
	return e.AnalyzeContext(context.Background(),
		current, previous, cfg, lockChainFindings)
}

// AnalyzeContext detects signals, produces Tier 1 incidents via decision
// trees, optionally correlates unexplained signals with the LLM (without
// holding the engine mutex), deduplicates, auto-resolves and escalates.
// ctx bounds every database and LLM call made during the cycle.
func (e *Engine) AnalyzeContext(
	ctx context.Context,
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) []Incident {
	e.cycleMu.Lock()
	defer e.cycleMu.Unlock()

	e.syncResolved(ctx)
	plan := e.prepareCycle(current, previous, cfg, lockChainFindings)
	if plan.tier2 != nil {
		plan.incidents = append(plan.incidents,
			e.runTier2(ctx, plan.tier2)...)
	}
	actions := e.fetchSageActions(ctx, plan)
	return e.commitCycle(plan, actions)
}

// ActiveIncidents returns a copy of all unresolved incidents.
func (e *Engine) ActiveIncidents() []Incident {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeIncidents()
}

func (e *Engine) activeIncidents() []Incident {
	active := make([]Incident, 0, len(e.incidents))
	for _, inc := range e.incidents {
		if inc.ResolvedAt == nil {
			active = append(active, inc)
		}
	}
	return active
}
