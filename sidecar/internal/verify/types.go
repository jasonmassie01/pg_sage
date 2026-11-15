package verify

import (
	"context"
	"errors"
	"time"
)

// Measurement summarizes one query (or a pool of queries) over a window:
// the calls, their call-weighted mean, the standard error of that mean
// across the window's time buckets, and how many buckets carried calls.
// StdErr is meaningful only with Buckets >= 2.
type Measurement struct {
	Samples        int           `json:"calls"`
	AverageLatency time.Duration `json:"mean_ns"`
	StdErr         time.Duration `json:"stderr_ns"`
	Buckets        int           `json:"buckets"`
}

type Criterion struct {
	Kind           string
	TargetIDs      []int64
	MinGainPct     float64
	RegressPct     float64
	WriteImpactPct float64
	Window         time.Duration
	HardMax        time.Duration
}

type WatchRequest struct {
	ID         string
	ActionID   int64
	ExecutedAt time.Time
	Table      string
	IndexName  string
	Criterion  Criterion
	// FrozenWeights are the targets' pre-period call shares
	// (FrozenWeights), fixed when the watch is created so the attribution's
	// pooled metric cannot move with the post-period call mix (lab-01a).
	FrozenWeights map[int64]float64
}

type WatchState struct {
	ID               string
	ActionID         int64
	ExecutedAt       time.Time
	Table            string
	IndexName        string
	Criterion        Criterion
	Status           string
	Reason           string
	Completed        bool
	Window           time.Duration
	NextEvaluationAt time.Time
	// ObservationFailures counts consecutive observations that could not
	// read their evidence; a successful observation resets it.
	ObservationFailures int
	// FrozenWeights are the request's; nil for a watch stored before
	// lab-01a (judged by v1 only).
	FrozenWeights map[int64]float64
	// AlphaSpend is the alpha the watch's looks have spent (lab-01b,
	// SpendLook); zero before its first look.
	AlphaSpend AlphaSpend
	// Other is the latest verdict of the verifier that did not act
	// (lab-01c); nil while only v1 judges.
	Other *OtherVerdict
}

// OtherVerdict is the verdict of the verifier that did not act: v2 in
// shadow, v1 in v2 (lab-01c). It is stored with the watch and in
// sage.action_outcome.other_verdict and other_evidence for comparison.
type OtherVerdict struct {
	Verifier string         `json:"verifier"`
	Outcome  string         `json:"outcome,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

type Verdict struct {
	Retain           bool
	Revert           bool
	Status           string
	Reason           string
	Samples          int
	Window           time.Duration
	NextEvaluationAt time.Time
	// Outcome is the Phase 1.3 verdict (improved, neutral, regressed,
	// insufficient_evidence, unverifiable); ObservedPct the call-weighted
	// change of the targets; Evidence the comparison behind it.
	Outcome     string
	ObservedPct *float64
	Evidence    map[string]any
	// ObservationFailures is the watch's consecutive failed observations
	// after this evaluation (0 when it read its evidence).
	ObservationFailures int
	// Verifier is the judge that decided this verdict (v1 or v2); Other the
	// other judge's verdict, and AlphaSpend the look's spend to store
	// (lab-01c). Both nil while only v1 judges.
	Verifier   string
	Other      *OtherVerdict
	AlphaSpend *AlphaSpend
}

// ErrStateNotPersisted marks a Watch that returned before its durable state
// existed. Only then is reverting the action the safe answer: once the watch
// is stored, a later resume observes it again.
var ErrStateNotPersisted = errors.New("verification state not persisted")

// ResumeResult preserves the durable watch identity alongside its verdict.
// Executors need this correlation to finalize or revert the correct action.
type ResumeResult struct {
	WatchID  string
	ActionID int64
	Verdict  Verdict
}

// Admission is a load-admission verdict. Mode names the evidence that
// admitted or withheld the action; Evidence is recorded in the decision.
type Admission struct {
	OK       bool
	Reason   string
	Mode     string
	Detail   string
	Evidence map[string]any
}

type ObservationSource interface {
	QueryMeasurements(context.Context, []int64, time.Time, time.Time) (
		map[int64]Measurement, error,
	)
	// WriteCost is the table-scoped write cost of one window (executor-03b).
	WriteCost(context.Context, string, time.Time, time.Time) (WriteCost, error)
	IndexValid(context.Context, string) (bool, error)
	LoadEvidence(context.Context) (LoadEvidence, error)
}

type StateStore interface {
	Create(context.Context, WatchState) error
	Update(context.Context, WatchState) error
	Get(context.Context, string) (WatchState, error)
	ListDue(context.Context, time.Time) ([]WatchState, error)
	// Claim takes exclusive ownership of a listed watch that is still due at
	// now by moving its next evaluation to leaseUntil, and returns the
	// current durable state. It reports false when the watch is no longer
	// due (another worker claimed or finished it). Ownership lapses at
	// leaseUntil, so a worker that died is retried after its lease.
	Claim(ctx context.Context, listed WatchState, now, leaseUntil time.Time) (
		WatchState, bool, error,
	)
}

type Options struct {
	InitialWindow    time.Duration
	HardMax          time.Duration
	MinSamples       int
	MinGainPct       float64
	RegressPct       float64
	WriteImpactPct   float64
	CPUCeilingPct    float64
	DataIOCeilingPct float64
	LogIOCeilingPct  float64
	// BaselineDays is the learned-baseline observation required before
	// admission; 0 disables the learned-baseline path.
	BaselineDays float64
	Now          func() time.Time
}

func DefaultOptions() Options {
	return Options{
		InitialWindow: 2 * time.Hour, HardMax: 72 * time.Hour, MinSamples: 30,
		MinGainPct: 20, RegressPct: 15, WriteImpactPct: 20,
		CPUCeilingPct: 70, DataIOCeilingPct: 70, LogIOCeilingPct: 70,
		BaselineDays: 7, Now: time.Now,
	}
}

type Engine struct {
	source  ObservationSource
	store   StateStore
	options Options
	v2      *V2Options
}

func NewEngine(source ObservationSource, store StateStore, options Options) (*Engine, error) {
	if source == nil || store == nil {
		return nil, errors.New("verify: observation source and state store are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Engine{source: source, store: store, options: options}, nil
}

func WatchStateFromRequest(request WatchRequest) WatchState {
	return WatchState{
		ID: request.ID, ActionID: request.ActionID, ExecutedAt: request.ExecutedAt,
		Table: request.Table, IndexName: request.IndexName, Criterion: request.Criterion,
		Status: "pending", Window: request.Criterion.Window,
		NextEvaluationAt: request.ExecutedAt.Add(request.Criterion.Window),
		FrozenWeights:    request.FrozenWeights,
	}
}
