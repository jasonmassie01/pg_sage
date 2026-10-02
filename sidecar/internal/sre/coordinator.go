package sre

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The coordinator (AI-SRE-SPEC §5, Codex §4/§7) is one database's
// investigation loop: committed triggers (RCA incidents, plan regression
// findings) become durable investigations; a worker claims one under a
// fenced lease, runs its fixed probe plan step by step, builds the causal
// diagnosis from the stored evidence and persists it. An optional model
// turn (M3) may rank the graph's hypotheses, ask for one catalog probe
// and narrate cited claims; it never decides the root cause, never
// executes an action and never fails an investigation. A failure here
// cannot block RCA, policy or the emergency stop.

// ProbeRunner runs one catalog probe (probes.Runner).
type ProbeRunner interface {
	Run(ctx context.Context, id probes.ID, args probes.Args) probes.Result
}

// Trigger is a committed incident signal (or an operator's request)
// that asks for an investigation. Actor is who asked: empty means the
// trigger loop.
type Trigger struct {
	CaseID         string
	IncidentID     string
	Kind           TriggerKind
	Subject        string
	IdempotencyKey string
	Actor          string
}

// TriggerSource yields the current committed triggers.
type TriggerSource interface {
	Triggers(ctx context.Context) ([]Trigger, error)
}

// CoordinatorStore is the store surface the coordinator needs: the
// durable model budget and model events serve the optional model turn.
type CoordinatorStore interface {
	ModelStore
	RecordEvent(ctx context.Context, lease Lease, typ string, payload map[string]any) error
	Limits() Limits
	EnsureDeployment(ctx context.Context) (UUID, error)
	BindDatabase(ctx context.Context, b Binding) (Scope, error)
	Create(ctx context.Context, req StartRequest) (Investigation, bool, error)
	Get(ctx context.Context, scope Scope, id UUID) (Investigation, error)
	Claim(ctx context.Context, scope Scope, id, worker UUID) (Lease, error)
	Heartbeat(ctx context.Context, lease Lease) (Lease, error)
	CommitStep(ctx context.Context, lease Lease, step StepResult) (Investigation, error)
	Conclude(ctx context.Context, lease Lease, c Conclusion) (Investigation, error)
	Evidence(ctx context.Context, scope Scope, id UUID) ([]Evidence, error)
	Pending(ctx context.Context, scope Scope, limit int) ([]UUID, error)
	Purge(ctx context.Context, scope Scope, p RetentionPolicy) (PurgeResult, error)
	Ping(ctx context.Context) error
}

var _ CoordinatorStore = (*PostgresStore)(nil)

// CoordinatorConfig is one database's investigator settings.
type CoordinatorConfig struct {
	// RuntimeKey is the stable connection-entry key bound to the database
	// UUID; LegacyDatabaseID is the meta-db record id, when there is one.
	RuntimeKey       string
	LegacyDatabaseID *int
	// AutomaticStart starts investigations from triggers; otherwise the
	// coordinator only resumes pending work and applies retention.
	AutomaticStart  bool
	TriggerInterval time.Duration
	SampleInterval  time.Duration
	QueueSize       int
	Retention       RetentionPolicy
	// RetentionInterval spaces retention passes.
	RetentionInterval time.Duration
	// ActionWindow is how far back pg_sage's own actions count.
	ActionWindow time.Duration
	// ModelTimeout caps one model turn; the investigation's remaining
	// active time caps it further.
	ModelTimeout time.Duration
}

// Coordinator limits.
const (
	MaxQueueSize      = 100
	MaxSampleInterval = 30 * time.Second
	pendingBatch      = 100
)

// DefaultCoordinatorConfig returns the documented defaults.
func DefaultCoordinatorConfig(runtimeKey string) CoordinatorConfig {
	return CoordinatorConfig{RuntimeKey: runtimeKey, TriggerInterval: 15 * time.Second,
		SampleInterval: 5 * time.Second, QueueSize: MaxQueueSize,
		Retention: RetentionPolicy{EvidenceAge: 30 * 24 * time.Hour,
			TimelineAge: 90 * 24 * time.Hour, BatchSize: 100},
		RetentionInterval: time.Hour, ActionWindow: time.Hour,
		ModelTimeout: DefaultModelTimeout}
}

// DefaultModelTimeout caps one model turn: two turns fit the 120 s
// active-time ceiling.
const DefaultModelTimeout = 50 * time.Second

func (c CoordinatorConfig) validate(hasTriggers bool) error {
	checks := []struct {
		ok      bool
		problem string
	}{
		{c.RuntimeKey != "" && len(c.RuntimeKey) <= 128, "runtime key must be 1-128 bytes"},
		{c.TriggerInterval > 0, "trigger interval must be positive"},
		{c.SampleInterval > 0 && c.SampleInterval <= MaxSampleInterval,
			"sample interval must be in (0, 30s]"},
		{c.QueueSize > 0 && c.QueueSize <= MaxQueueSize, "queue size must be 1-100"},
		{c.RetentionInterval > 0, "retention interval must be positive"},
		{c.ActionWindow >= time.Minute && c.ActionWindow <= probes.MaxWindow,
			"action window must be in [1m, 7d]"},
		{!c.AutomaticStart || hasTriggers, "automatic start needs a trigger source"},
		{c.ModelTimeout > 0 && c.ModelTimeout <= CeilingActive,
			"model timeout must be in (0, 120s]"},
	}
	for _, ch := range checks {
		if !ch.ok {
			return fmt.Errorf("%w: %s", ErrInvalidRequest, ch.problem)
		}
	}
	return c.Retention.validate()
}

// CoordinatorDeps wires a coordinator.
type CoordinatorDeps struct {
	Store    CoordinatorStore
	Runner   ProbeRunner
	Triggers TriggerSource
	Config   CoordinatorConfig
	LogFn    func(level, msg string, args ...any)
	// Wait waits between compared samples; nil sleeps. PGIncidentBench
	// runs a scenario's mid-sample fault program here.
	Wait func(ctx context.Context, d time.Duration) error
	// Model is the optional LLM the model turn consults; nil keeps every
	// investigation deterministic.
	Model *llm.Client
	// Notices says once that the model turn is unavailable; nil uses the
	// process-wide ModelNotices.
	Notices *OnceLog
	// Signals (M5) are the change feed and SLO status sources every
	// investigation also collects; nil keeps the M2 plans.
	Signals []SignalProbe
}

// Coordinator runs one database's investigations.
type Coordinator struct {
	store      CoordinatorStore
	runner     ProbeRunner
	triggers   TriggerSource
	cfg        CoordinatorConfig
	logFn      func(level, msg string, args ...any)
	worker     UUID
	queue      chan UUID
	durability *Durability
	sleep      func(ctx context.Context, d time.Duration) error
	model      *llm.Client
	notices    *OnceLog
	signals    []probes.ID

	mu    sync.Mutex
	scope Scope
	bound bool
}

// NewCoordinator validates its dependencies and configuration.
func NewCoordinator(d CoordinatorDeps) (*Coordinator, error) {
	if d.Store == nil || d.Runner == nil {
		return nil, fmt.Errorf("%w: coordinator needs a store and a probe runner",
			ErrInvalidRequest)
	}
	if err := d.Config.validate(d.Triggers != nil); err != nil {
		return nil, err
	}
	if err := validateSignals(d.Signals); err != nil {
		return nil, err
	}
	logFn := d.LogFn
	if logFn == nil {
		logFn = func(string, string, ...any) {}
	}
	wait := d.Wait
	if wait == nil {
		wait = sleepCtx
	}
	notices := d.Notices
	if notices == nil {
		notices = ModelNotices
	}
	return &Coordinator{store: d.Store, runner: withSignals(d.Runner, d.Signals),
		triggers: d.Triggers, signals: signalIDs(d.Signals),
		cfg: d.Config, logFn: logFn, worker: NewUUID(),
		queue: make(chan UUID, d.Config.QueueSize), durability: NewDurability(),
		sleep: wait, model: d.Model, notices: notices}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Bind resolves this database's scope (deployment and database UUIDs)
// once; the binding survives restarts (the runtime key is stable).
func (c *Coordinator) Bind(ctx context.Context) (Scope, error) {
	if s, ok := c.Scope(); ok {
		return s, nil
	}
	dep, err := c.store.EnsureDeployment(ctx)
	if err == nil {
		var scope Scope
		scope, err = c.store.BindDatabase(ctx, Binding{DeploymentID: dep,
			RuntimeKey: c.cfg.RuntimeKey, LegacyDatabaseID: c.cfg.LegacyDatabaseID,
			Strength: StrengthConfigured, ClusterEpoch: "unknown"})
		if err == nil {
			c.mu.Lock()
			c.scope, c.bound = scope, true
			c.mu.Unlock()
			return scope, nil
		}
	}
	c.durability.Observe(err)
	return Scope{}, err
}

// Scope returns the bound scope, if bound.
func (c *Coordinator) Scope() (Scope, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scope, c.bound
}

// Durability is the coordinator's metadata-durability guard.
func (c *Coordinator) Durability() *Durability { return c.durability }

// Start creates (or coalesces into) the investigation of a trigger and
// queues it for the worker when it is waiting to run.
func (c *Coordinator) Start(ctx context.Context, t Trigger) (Investigation, bool, error) {
	scope, ok := c.Scope()
	if !ok {
		return Investigation{}, false, fmt.Errorf("%w: coordinator is not bound",
			ErrInvalidRequest)
	}
	actor := t.Actor
	if actor == "" {
		actor = "trigger"
	}
	inv, created, err := c.store.Create(ctx, StartRequest{Scope: scope, CaseID: t.CaseID,
		IncidentID: t.IncidentID, TriggerKind: t.Kind, Subject: t.Subject,
		IdempotencyKey: t.IdempotencyKey, Actor: actor})
	c.durability.Observe(err)
	if err != nil {
		return inv, false, err
	}
	if inv.State == StateQueued {
		c.enqueue(inv.ID)
	}
	return inv, created, nil
}

// enqueue queues work without blocking; a full queue drops the id, and
// the next pending scan finds it again.
func (c *Coordinator) enqueue(id UUID) {
	select {
	case c.queue <- id:
	default:
		c.logFn("WARN", "sre: investigation queue full; %s waits for the next scan", id)
	}
}
