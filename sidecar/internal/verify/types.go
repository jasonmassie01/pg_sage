package verify

import (
	"context"
	"errors"
	"time"
)

type Measurement struct {
	Samples        int
	AverageLatency time.Duration
}

type LoadSample struct {
	CPUPct    float64
	DataIOPct float64
	LogIOPct  float64
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
}

type Verdict struct {
	Retain           bool
	Revert           bool
	Status           string
	Reason           string
	Samples          int
	Window           time.Duration
	NextEvaluationAt time.Time
}

// ResumeResult preserves the durable watch identity alongside its verdict.
// Executors need this correlation to finalize or revert the correct action.
type ResumeResult struct {
	WatchID  string
	ActionID int64
	Verdict  Verdict
}

type Admission struct {
	OK     bool
	Reason string
}

type ObservationSource interface {
	QueryMeasurements(context.Context, []int64, time.Time, time.Time) (
		map[int64]Measurement, error,
	)
	WriteMeasurements(context.Context, string, time.Time, time.Time) (Measurement, error)
	IndexValid(context.Context, string) (bool, error)
	CurrentLoad(context.Context) (LoadSample, error)
}

type StateStore interface {
	Create(context.Context, WatchState) error
	Update(context.Context, WatchState) error
	Get(context.Context, string) (WatchState, error)
	ListDue(context.Context, time.Time) ([]WatchState, error)
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
	Now              func() time.Time
}

func DefaultOptions() Options {
	return Options{
		InitialWindow: 2 * time.Hour, HardMax: 72 * time.Hour, MinSamples: 30,
		MinGainPct: 20, RegressPct: 15, WriteImpactPct: 20,
		CPUCeilingPct: 70, DataIOCeilingPct: 70, LogIOCeilingPct: 70,
		Now: time.Now,
	}
}

type Engine struct {
	source  ObservationSource
	store   StateStore
	options Options
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
	}
}
