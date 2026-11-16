// Package changeplan holds pg_sage's plans: durable, multi-step,
// evidence-bound work in which every step is an ordinary typed action that
// runs through the executor (plan-01a). This file holds the plan and step
// objects; validate.go checks them, hash.go binds an approval to them and
// store.go keeps them in the monitored database's sage schema (sage.plan,
// sage.plan_step, sage.plan_step_edge, sage.plan_approval).
//
// Nothing here writes to a monitored database's user objects: the engine
// (plan-02a) queues each step as an approved action_queue item.
package changeplan

import "time"

// Origin is who proposed a plan.
type Origin string

const (
	OriginGenerator Origin = "generator"
	OriginModel     Origin = "model"
	OriginPerson    Origin = "person"
	OriginAgent     Origin = "agent"
)

// Status is a plan's lifecycle state. Completed, reverted, rejected,
// voided and cancelled are terminal (retention ages them out).
type Status string

const (
	StatusDraft     Status = "draft"
	StatusProposed  Status = "proposed"
	StatusApproved  Status = "approved"
	StatusRunning   Status = "running"
	StatusHalted    Status = "halted"
	StatusCompleted Status = "completed"
	StatusReverted  Status = "reverted"
	StatusRejected  Status = "rejected"
	StatusVoided    Status = "voided"
	StatusCancelled Status = "cancelled"
)

// StepKind is what a step is: an executor action, a step an operator runs
// ("you run this", plan-02d) or a read-only check.
type StepKind string

const (
	KindAction   StepKind = "action"
	KindOperator StepKind = "operator"
	KindCheck    StepKind = "check"
)

// Metric is what a step's verdict measures on its target queries;
// readback reads a setting back instead.
type Metric string

const (
	MetricBlocksPerCall Metric = "blocks_per_call"
	MetricTimePerCall   Metric = "time_per_call"
	MetricWALPerTuple   Metric = "wal_per_tuple"
	MetricReadback      Metric = "readback"
)

// Direction is which way a step is expected to move its metric.
type Direction string

const (
	DirectionDown Direction = "down"
	DirectionUp   Direction = "up"
	DirectionNone Direction = "none"
)

// ExpectedSource is where a step's expected size comes from (the ladder);
// none declares a step that cannot be measured.
type ExpectedSource string

const (
	SourceHypoPG ExpectedSource = "hypopg"
	SourceClone  ExpectedSource = "clone"
	SourceCanary ExpectedSource = "canary"
	SourceNone   ExpectedSource = "none"
)

// OutcomeAction is what the engine does after a step's verdict.
type OutcomeAction string

const (
	ActionKeep               OutcomeAction = "keep"
	ActionRevertStep         OutcomeAction = "revert_step"
	ActionRevertToCheckpoint OutcomeAction = "revert_to_checkpoint"
	ActionHaltAsk            OutcomeAction = "halt_ask"
)

// Outcome is a step's verdict, in lab-01's vocabulary plus expired.
type Outcome string

const (
	OutcomeImproved     Outcome = "improved"
	OutcomeNoChange     Outcome = "no_change"
	OutcomeRegressed    Outcome = "regressed"
	OutcomeInconclusive Outcome = "inconclusive"
	OutcomeUnverifiable Outcome = "unverifiable"
	OutcomeExpired      Outcome = "expired"
)

// StepState is a step's runtime state (outside the hash).
type StepState string

// The full step-state vocabulary, set up front so later engine items add
// no CHECK change.
const (
	StatePending          StepState = "pending"
	StateStarting         StepState = "starting"
	StateQueued           StepState = "queued"
	StateRunning          StepState = "running"
	StateExecuted         StepState = "executed"
	StateJudging          StepState = "judging"
	StateKept             StepState = "kept"
	StateReverting        StepState = "reverting"
	StateReverted         StepState = "reverted"
	StateSkipped          StepState = "skipped"
	StateFailed           StepState = "failed"
	StateAwaitingOperator StepState = "awaiting_operator"
	StateHalted           StepState = "halted"
)

// Scope is what a plan may touch: object names and query ids.
type Scope struct {
	Objects  []string `json:"objects,omitempty"`
	QueryIDs []int64  `json:"queryids,omitempty"`
}

// Guardrail is a secondary metric a step must not worsen by more than
// MaxRegressPct percent.
type Guardrail struct {
	Metric        Metric  `json:"metric"`
	MaxRegressPct float64 `json:"max_regress_pct"`
}

// MinEvidence is how much a step's targets must run for a verdict; zero
// fields take DefaultMinCalls and DefaultMinSamples.
type MinEvidence struct {
	MinCalls   int64 `json:"min_calls,omitempty"`
	MinSamples int   `json:"min_samples,omitempty"`
}

// Step is one plan step. Fields above Runtime are what an approval binds
// (the plan hash); Runtime is the engine's and is never hashed.
type Step struct {
	ID                int64          `json:"id,omitempty"`
	Ordinal           int            `json:"ordinal"`
	Kind              StepKind       `json:"kind"`
	ActionType        string         `json:"action_type,omitempty"`
	Setting           string         `json:"setting,omitempty"`
	ProposedSQL       string         `json:"proposed_sql,omitempty"`
	RollbackSQL       string         `json:"rollback_sql,omitempty"`
	FindingID         *int64         `json:"finding_id,omitempty"`
	EvidenceIDs       []string       `json:"evidence_ids,omitempty"`
	TargetQueryIDs    []int64        `json:"target_queryids,omitempty"`
	Metric            Metric         `json:"metric"`
	ExpectedDirection Direction      `json:"expected_direction"`
	ExpectedPct       *float64       `json:"expected_pct,omitempty"`
	ExpectedSource    ExpectedSource `json:"expected_source"`
	Guardrails        []Guardrail    `json:"guardrails,omitempty"`
	MinEvidence       MinEvidence    `json:"min_evidence"`
	DeadlineMinutes   int            `json:"deadline_minutes"`
	Checkpoint        bool           `json:"checkpoint"`
	OnImproved        OutcomeAction  `json:"on_improved,omitempty"`
	OnNoChange        OutcomeAction  `json:"on_no_change,omitempty"`
	OnRegressed       OutcomeAction  `json:"on_regressed,omitempty"`
	OnInconclusive    OutcomeAction  `json:"on_inconclusive,omitempty"`
	// OnUnverifiable empty means undeclared: an unverifiable verdict then
	// halts and asks. Only a declared keep keeps an unmeasurable step.
	OnUnverifiable OutcomeAction `json:"on_unverifiable,omitempty"`
	OnExpired      OutcomeAction `json:"on_expired,omitempty"`
	Runtime        StepRuntime   `json:"runtime"`
}

// StepRuntime is the engine's view of a step. It links the step to the
// ledger on the plan side, so no chained table gains a column.
type StepRuntime struct {
	State       StepState  `json:"state"`
	QueueID     *int64     `json:"queue_id,omitempty"`
	ActionLogID *int64     `json:"action_log_id,omitempty"`
	DecisionID  *int64     `json:"decision_id,omitempty"`
	Verdict     Outcome    `json:"verdict,omitempty"`
	VerdictAt   *time.Time `json:"verdict_at,omitempty"`
	StateReason string     `json:"state_reason,omitempty"`
	// ClaimedBy and ClaimedUntil are the engine running the step and when
	// its claim lapses; Attempt counts its starts; ExecutedAt is when its
	// action ran, from which DeadlineMinutes counts (plan-02a).
	ClaimedBy    string     `json:"claimed_by,omitempty"`
	ClaimedUntil *time.Time `json:"claimed_until,omitempty"`
	Attempt      int        `json:"attempt,omitempty"`
	ExecutedAt   *time.Time `json:"executed_at,omitempty"`
}

// Edge says the step at From must finish before the step at To starts.
// Both are ordinals of the same plan's steps.
type Edge struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// Plan is a plan with its steps (in ordinal order) and edges.
type Plan struct {
	ID            int64     `json:"id,omitempty"`
	DatabaseID    *int      `json:"database_id,omitempty"`
	Goal          string    `json:"goal"`
	Scope         Scope     `json:"scope"`
	Origin        Origin    `json:"origin"`
	OriginRef     string    `json:"origin_ref,omitempty"`
	PackHash      string    `json:"pack_hash,omitempty"`
	Model         string    `json:"model,omitempty"`
	PromptVersion string    `json:"prompt_version,omitempty"`
	Status        Status    `json:"status"`
	PlanHash      string    `json:"plan_hash,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	CreatedBy     string    `json:"created_by"`
	// PauseReason says why a running plan is not advancing ("emergency
	// stop"); empty otherwise. It is the engine's and is never hashed.
	PauseReason string `json:"pause_reason,omitempty"`
	Steps       []Step `json:"steps"`
	Edges       []Edge `json:"edges"`
}

// Defaults for a step's minimum evidence (verify.min_samples' default).
const (
	DefaultMinCalls   int64 = 30
	DefaultMinSamples       = 30
)

// defaultActions are the outcome actions a step gets when it names none.
// Regressed reverts the step (and the engine halts the plan); an
// unverifiable step halts unless it declared otherwise before it ran; an
// expired monitor is never closed silently.
var defaultActions = map[Outcome]OutcomeAction{
	OutcomeImproved:     ActionKeep,
	OutcomeNoChange:     ActionKeep,
	OutcomeRegressed:    ActionRevertStep,
	OutcomeInconclusive: ActionHaltAsk,
	OutcomeUnverifiable: ActionHaltAsk,
	OutcomeExpired:      ActionHaltAsk,
}

// ActionFor is what the engine does on outcome o: the step's own action,
// else the default. An unknown outcome halts and asks (fail closed).
func (s Step) ActionFor(o Outcome) OutcomeAction {
	if a := s.actionField(o); a != "" {
		return a
	}
	if a, ok := defaultActions[o]; ok {
		return a
	}
	return ActionHaltAsk
}

func (s Step) actionField(o Outcome) OutcomeAction {
	switch o {
	case OutcomeImproved:
		return s.OnImproved
	case OutcomeNoChange:
		return s.OnNoChange
	case OutcomeRegressed:
		return s.OnRegressed
	case OutcomeInconclusive:
		return s.OnInconclusive
	case OutcomeUnverifiable:
		return s.OnUnverifiable
	case OutcomeExpired:
		return s.OnExpired
	}
	return ""
}

// WithDefaults fills every outcome action except OnUnverifiable (whose
// emptiness means "undeclared") and the minimum evidence. Store.Create
// applies it, so the stored rows and the hash hold the effective values.
func (s Step) WithDefaults() Step {
	fill := func(a *OutcomeAction, o Outcome) {
		if *a == "" {
			*a = defaultActions[o]
		}
	}
	fill(&s.OnImproved, OutcomeImproved)
	fill(&s.OnNoChange, OutcomeNoChange)
	fill(&s.OnRegressed, OutcomeRegressed)
	fill(&s.OnInconclusive, OutcomeInconclusive)
	fill(&s.OnExpired, OutcomeExpired)
	if s.MinEvidence.MinCalls == 0 {
		s.MinEvidence.MinCalls = DefaultMinCalls
	}
	if s.MinEvidence.MinSamples == 0 {
		s.MinEvidence.MinSamples = DefaultMinSamples
	}
	if s.Runtime.State == "" {
		s.Runtime.State = StatePending
	}
	return s
}

// MapVerifierOutcome maps a stored verify.Verdict.Outcome to the plan
// vocabulary: neutral is no_change, insufficient_evidence is
// inconclusive; any other string is inconclusive, which halts (fail
// closed).
func MapVerifierOutcome(outcome string) Outcome {
	switch outcome {
	case "improved":
		return OutcomeImproved
	case "neutral":
		return OutcomeNoChange
	case "regressed":
		return OutcomeRegressed
	case "unverifiable":
		return OutcomeUnverifiable
	case "expired":
		return OutcomeExpired
	}
	return OutcomeInconclusive
}

// Terminal reports whether s is a terminal plan status.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusReverted, StatusRejected, StatusVoided, StatusCancelled:
		return true
	}
	return false
}
