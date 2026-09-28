package sre

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Investigation is the durable state of one investigation.
type Investigation struct {
	Scope           Scope
	ID              UUID
	CaseID          string
	TriggerKind     TriggerKind
	State           State
	Version         int64
	Fence           int64
	LeaseOwner      UUID
	LeaseUntil      time.Time
	SegmentDeadline time.Time
	ActiveMS        int64
	ProbeCount      int
	ModelTurns      int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       time.Time
	FailureCode     string
	// M2: the trigger's incident and subject, operator pinning, the
	// persisted diagnosis summary and retention markers.
	IncidentID       string
	Subject          string
	Pinned           bool
	Summary          Summary
	ConcludedAt      time.Time
	EvidencePurgedAt time.Time
}

// Lease is a worker's fenced claim on an investigation. Every write
// compares the worker, fence and lease expiry; a stale worker cannot
// commit.
type Lease struct {
	Scope           Scope
	InvestigationID UUID
	WorkerID        UUID
	Version         int64
	Fence           int64
	Until           time.Time
	SegmentDeadline time.Time
}

func (l Lease) validate() error {
	if err := l.Scope.Validate(); err != nil {
		return err
	}
	if _, err := ParseUUID(string(l.InvestigationID)); err != nil {
		return err
	}
	if _, err := ParseUUID(string(l.WorkerID)); err != nil {
		return err
	}
	if l.Fence <= 0 {
		return ErrInvalidRequest
	}
	return nil
}

// StepResult is one idempotent investigation step: the probe results it
// observed (stored as immutable evidence) and the next state.
type StepResult struct {
	IdempotencyKey string
	Results        []probes.Result
	NextState      State
	ErrorCode      string
}

// Evidence is one immutable, hashed probe observation.
type Evidence struct {
	ID              UUID
	StepID          UUID
	StepKey         string
	ProbeID         string
	ProbeVersion    string
	CapabilityState string
	ReasonCode      string
	ObservedAt      time.Time
	CollectedAt     time.Time
	Payload         []byte
	SHA256          []byte
}

// ReservationState is the lifecycle of a durable model reservation.
type ReservationState string

// Reservation states (Codex contracts §6).
const (
	ReservationReserved  ReservationState = "reserved"
	ReservationInflight  ReservationState = "inflight"
	ReservationSettled   ReservationState = "settled"
	ReservationUnknown   ReservationState = "unknown"
	ReservationCancelled ReservationState = "cancelled"
)

// TokenRequest asks to reserve model tokens for one turn.
type TokenRequest struct {
	Input, Output int64
	RequestKey    string
}

// Reservation is a durable model-budget hold.
type Reservation struct {
	ID              UUID
	Scope           Scope
	InvestigationID UUID
	RequestKey      string
	State           ReservationState
	Input, Output   int64
	InputUsed       int64
	OutputUsed      int64
	Version         int64
}

// Usage is a provider's reported usage; Known false means the call's
// outcome is uncertain and its full reservation stays held.
type Usage struct {
	Input, Output int64
	Known         bool
}

// ModelStore is the durable model-budget half of the store.
type ModelStore interface {
	ReserveModel(ctx context.Context, lease Lease, req TokenRequest) (Reservation, error)
	MarkDispatched(ctx context.Context, scope Scope, res Reservation) (Reservation, error)
	SettleModel(ctx context.Context, scope Scope, res Reservation, u Usage) (Reservation, error)
	CancelModel(ctx context.Context, scope Scope, res Reservation) (Reservation, error)
}

// Store is the durable coordination store: the meta database when one
// is configured, the monitored database otherwise (AI-SRE-SPEC §5).
type Store interface {
	ModelStore
	EnsureDeployment(ctx context.Context) (UUID, error)
	BindDatabase(ctx context.Context, b Binding) (Scope, error)
	Create(ctx context.Context, req StartRequest) (Investigation, bool, error)
	Get(ctx context.Context, scope Scope, id UUID) (Investigation, error)
	Claim(ctx context.Context, scope Scope, id, worker UUID) (Lease, error)
	Heartbeat(ctx context.Context, lease Lease) (Lease, error)
	CommitStep(ctx context.Context, lease Lease, step StepResult) (Investigation, error)
	Release(ctx context.Context, lease Lease, next State) (Investigation, error)
	Pause(ctx context.Context, scope Scope, id UUID, version int64) (Investigation, error)
	Resume(ctx context.Context, scope Scope, id UUID, version int64) (Investigation, error)
	Stop(ctx context.Context, scope Scope, id UUID, version int64) (Investigation, error)
	Evidence(ctx context.Context, scope Scope, id UUID) ([]Evidence, error)
	Ping(ctx context.Context) error
}
