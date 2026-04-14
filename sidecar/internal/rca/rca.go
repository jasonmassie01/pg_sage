package rca

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
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
}

// ChainLink is one step in the causal chain leading to an incident.
type ChainLink struct {
	Order       int    `json:"order"`
	Signal      string `json:"signal"`
	Description string `json:"description"`
	Evidence    string `json:"evidence"`
}

// Signal is a single fired detector result.
type Signal struct {
	ID       string         `json:"id"`
	FiredAt  time.Time      `json:"fired_at"`
	Severity string         `json:"severity"`
	Metrics  map[string]any `json:"metrics"`
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

// Engine is the root cause analysis engine supporting Tier 1
// (deterministic decision trees) and Tier 2 (LLM correlation).
type Engine struct {
	cfg             *config.RCAConfig
	incidents       []Incident
	clearCounts     map[string]int // incidentID -> consecutive clear cycles
	cycleCount      int
	gracePeriodLeft int
	logFn           func(string, string, ...any)
	llmClient       *llm.Client
	logSource       LogSource
	actionStore     ActionQuerier
	correlator      *SelfActionCorrelator
	mu              sync.Mutex
}

// WithLLM attaches an LLM client to enable Tier 2 correlation.
// Safe to call on a nil client — Tier 2 simply stays disabled.
func (e *Engine) WithLLM(client *llm.Client) {
	e.llmClient = client
}

// SetLogSource attaches a log-based signal source (logwatch adapter).
// When set, Analyze drains log signals each cycle.
func (e *Engine) SetLogSource(src LogSource) {
	e.logSource = src
}

// WithActionStore enables self-action correlation by wiring in a
// store that can query sage.action_log for recent actions and
// rollback history.
func (e *Engine) WithActionStore(store ActionQuerier) {
	e.actionStore = store
	e.correlator = NewSelfActionCorrelator(e.logFn)
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
		gracePeriodLeft: cfg.ResolutionCycles + 1,
		logFn:           logFn,
	}
}

// Analyze runs after every analyzer cycle. It detects signals, produces
// Tier 1 incidents via decision trees, deduplicates, auto-resolves, and
// escalates long-running incidents.
func (e *Engine) Analyze(
	current *collector.Snapshot,
	previous *collector.Snapshot,
	cfg *config.Config,
	lockChainFindings []analyzer.Finding,
) []Incident {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.cycleCount++
	if e.gracePeriodLeft > 0 {
		e.gracePeriodLeft--
	}

	signals := e.detectSignals(current, previous, cfg, lockChainFindings)

	if e.logSource != nil {
		logSignals := e.logSource.Drain()
		signals = append(signals, logSignals...)
	}

	firedIDs := make(map[string]bool, len(signals))
	for _, s := range signals {
		firedIDs[s.ID] = true
	}

	newIncidents := e.runDecisionTrees(signals, current, previous, cfg)

	logIncidents := e.runLogDecisionTrees(signals)
	newIncidents = append(newIncidents, logIncidents...)

	tier2 := e.runTier2Correlation(signals, newIncidents)
	newIncidents = append(newIncidents, tier2...)

	for i := range newIncidents {
		e.dedup(&newIncidents[i])
	}

	if e.correlator != nil && e.actionStore != nil {
		e.applySelfActionCorrelation(newIncidents)
	}

	e.autoResolve(firedIDs)
	e.escalate()

	return e.activeIncidents()
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

// ---------------------------------------------------------------------------
// Dedup, auto-resolve, escalate
// ---------------------------------------------------------------------------

func (e *Engine) dedup(inc *Incident) {
	window := time.Duration(e.cfg.DedupWindowMinutes) * time.Minute
	if window == 0 {
		window = 30 * time.Minute
	}
	normalizedIDs := sortedCopy(inc.SignalIDs)
	firstObj := ""
	if len(inc.AffectedObjects) > 0 {
		firstObj = inc.AffectedObjects[0]
	}

	for i := range e.incidents {
		existing := &e.incidents[i]
		if existing.ResolvedAt != nil {
			continue
		}
		if existing.Source != inc.Source {
			continue
		}
		existingIDs := sortedCopy(existing.SignalIDs)
		if !stringsEqual(existingIDs, normalizedIDs) {
			continue
		}
		existingFirst := ""
		if len(existing.AffectedObjects) > 0 {
			existingFirst = existing.AffectedObjects[0]
		}
		if existingFirst != firstObj {
			continue
		}
		lastSeen := existing.LastDetectedAt
		if lastSeen.IsZero() {
			lastSeen = existing.DetectedAt
		}
		if inc.DetectedAt.Sub(lastSeen) > window {
			continue
		}
		// Match found: update existing incident.
		existing.OccurrenceCount++
		existing.LastDetectedAt = inc.DetectedAt
		if severityRank(inc.Severity) > severityRank(existing.Severity) {
			existing.Severity = inc.Severity
		}
		// Reset clear counter since the incident fired again.
		delete(e.clearCounts, existing.ID)
		return
	}

	// No match: insert new incident.
	inc.ID = newUUID()
	inc.OccurrenceCount = 1
	inc.LastDetectedAt = inc.DetectedAt
	e.incidents = append(e.incidents, *inc)
}

func (e *Engine) autoResolve(firedIDs map[string]bool) {
	if e.gracePeriodLeft > 0 {
		return
	}
	needed := e.cfg.ResolutionCycles
	if needed == 0 {
		needed = 2
	}

	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt != nil {
			continue
		}
		stillFiring := false
		for _, sid := range inc.SignalIDs {
			if firedIDs[sid] {
				stillFiring = true
				break
			}
		}
		if stillFiring {
			delete(e.clearCounts, inc.ID)
			continue
		}
		e.clearCounts[inc.ID]++
		if e.clearCounts[inc.ID] >= needed {
			now := time.Now()
			inc.ResolvedAt = &now
			delete(e.clearCounts, inc.ID)
			e.logFn("info", "rca: auto-resolved incident %s (%s)",
				inc.ID, inc.RootCause)
		}
	}
}

func (e *Engine) escalate() {
	needed := e.cfg.EscalationCycles
	if needed == 0 {
		needed = 5
	}
	for i := range e.incidents {
		inc := &e.incidents[i]
		if inc.ResolvedAt != nil || inc.EscalatedAt != nil {
			continue
		}
		if inc.Severity != "warning" {
			continue
		}
		if inc.OccurrenceCount >= needed {
			now := time.Now()
			inc.Severity = "critical"
			inc.EscalatedAt = &now
			e.logFn("warn", "rca: escalated incident %s to critical (%s)",
				inc.ID, inc.RootCause)
		}
	}
}

// ---------------------------------------------------------------------------
// Self-action correlation
// ---------------------------------------------------------------------------

// applySelfActionCorrelation queries recent actions and rollback
// history, then correlates them with new incidents. Any self-caused
// or manual-review incidents are deduped and added to e.incidents.
func (e *Engine) applySelfActionCorrelation(newIncidents []Incident) {
	ctx := context.Background()

	recentActions, err := e.actionStore.RecentSageActions(
		ctx, 30*time.Minute)
	if err != nil {
		e.logFn("warn",
			"rca: failed to query recent actions: %v", err)
		return
	}

	rollbackHistory, err := e.actionStore.RollbackHistory(
		ctx, 30*24*time.Hour)
	if err != nil {
		e.logFn("warn",
			"rca: failed to query rollback history: %v", err)
		return
	}

	if len(recentActions) == 0 {
		return
	}

	_, selfCaused, manualReview := e.correlator.Correlate(
		newIncidents, recentActions, rollbackHistory)

	for i := range selfCaused {
		e.logFn("warn",
			"rca: self-caused incident: %s", selfCaused[i].RootCause)
		e.dedup(&selfCaused[i])
	}
	for i := range manualReview {
		e.logFn("warn",
			"rca: manual review required: %s",
			manualReview[i].RootCause)
		e.dedup(&manualReview[i])
	}
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

const upsertSQL = `
INSERT INTO sage.incidents (
    id, detected_at, last_detected_at, severity, root_cause,
    causal_chain, affected_objects, signal_ids, recommended_sql,
    rollback_sql, action_risk, source, confidence, resolved_at,
    database_name, occurrence_count, escalated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
ON CONFLICT (id) DO UPDATE SET
    severity         = EXCLUDED.severity,
    root_cause       = EXCLUDED.root_cause,
    causal_chain     = EXCLUDED.causal_chain,
    last_detected_at = EXCLUDED.last_detected_at,
    resolved_at      = EXCLUDED.resolved_at,
    occurrence_count = EXCLUDED.occurrence_count,
    escalated_at     = EXCLUDED.escalated_at`

// PersistIncidents upserts all tracked incidents to sage.incidents.
func (e *Engine) PersistIncidents(
	ctx context.Context,
	pool *pgxpool.Pool,
) error {
	e.mu.Lock()
	snapshot := make([]Incident, len(e.incidents))
	copy(snapshot, e.incidents)
	e.mu.Unlock()

	for _, inc := range snapshot {
		chainJSON := marshalChain(inc.CausalChain)
		_, err := pool.Exec(ctx, upsertSQL,
			inc.ID, inc.DetectedAt, inc.LastDetectedAt,
			inc.Severity, inc.RootCause,
			chainJSON, inc.AffectedObjects, inc.SignalIDs,
			inc.RecommendedSQL, inc.RollbackSQL, inc.ActionRisk,
			inc.Source, inc.Confidence, inc.ResolvedAt,
			inc.DatabaseName, inc.OccurrenceCount, inc.EscalatedAt,
		)
		if err != nil {
			return fmt.Errorf("rca: upsert incident %s: %w", inc.ID, err)
		}
	}
	return nil
}
