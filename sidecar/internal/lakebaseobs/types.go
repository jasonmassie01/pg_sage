// Package lakebaseobs ingests resource-scoped Lakebase system-table history.
// These asynchronous observations are never a live host-admission source.
package lakebaseobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Stream string

const (
	Plans             Stream = "plan_history"
	Logs              Stream = "postgres_logs"
	ASH               Stream = "active_session_history"
	DDL               Stream = "ddl_history"
	ComputeCounters   Stream = "compute_counters"
	ComputeGauges     Stream = "compute_gauges"
	DatabaseCounters  Stream = "database_counters"
	DatabaseGauges    Stream = "database_gauges"
	WaitCounters      Stream = "wait_event_counters"
	StatementCounters Stream = "pg_stat_statements_counters"
	SchemaVersion            = 1
	SourceRetention          = 7 * 24 * time.Hour
)

var (
	ErrScope       = errors.New("lakebase telemetry scope invalid")
	ErrSchema      = errors.New("lakebase telemetry schema unsupported")
	ErrBudget      = errors.New("lakebase telemetry result budget exceeded")
	ErrConflict    = errors.New("lakebase telemetry cursor changed")
	ErrMasked      = errors.New("lakebase telemetry payload unavailable or masked")
	ErrMalformed   = errors.New("lakebase telemetry row malformed")
	ErrPolicy      = errors.New("lakebase telemetry delivery policy revoked")
	ErrStalePolicy = errors.New("lakebase telemetry policy owner is an older process")
)

// Scope is a trusted binding, not inferred from query text or display names.
// DeploymentID is the persisted installation UUID across App snapshot redeploys,
// never the transient Apps deployment ID. DatabaseID is the registered SRE
// database UUID, not the Postgres OID.
type Scope struct {
	DeploymentID string `json:"deployment_id"`
	DatabaseID   string `json:"database_id"`
	Workspace    string `json:"workspace"`
	ProjectID    string `json:"project_id"`
	BranchID     string `json:"branch_id"`
	EndpointID   string `json:"endpoint_id"`
	DatabaseName string `json:"database_name"`
	DatabaseOID  int64  `json:"database_oid"`
	// Optional trusted narrowing for controlled payload acceptance. Streams
	// without application attribution cannot return payload under this scope.
	ApplicationName string `json:"application_name,omitempty"`
	// PolicyBinding hashes the trusted configured endpoint/host/database tuple.
	// It lets cleanup retain an earlier verified scope during SDK outages.
	PolicyBinding string `json:"policy_binding,omitempty"`
	AllowCompute  bool   `json:"allow_compute"`
}

func (s Scope) Validate() error {
	u, err := url.Parse(s.Workspace)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("%w: workspace must be an HTTPS origin", ErrScope)
	}
	if id, err := uuid.Parse(s.DeploymentID); err != nil || id == uuid.Nil {
		return fmt.Errorf("%w: deployment UUID required", ErrScope)
	}
	if id, err := uuid.Parse(s.DatabaseID); err != nil || id == uuid.Nil {
		return fmt.Errorf("%w: database UUID required", ErrScope)
	}
	for _, value := range []string{s.ProjectID, s.BranchID, s.EndpointID, s.DatabaseName} {
		if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
			return ErrScope
		}
	}
	if s.DatabaseOID <= 0 || s.DatabaseOID > 1<<32-1 {
		return fmt.Errorf("%w: database OID required", ErrScope)
	}
	if len(s.ApplicationName) > 256 || strings.ContainsAny(s.ApplicationName, "\x00\r\n") {
		return ErrScope
	}
	if s.PolicyBinding != "" {
		if len(s.PolicyBinding) != 64 {
			return ErrScope
		}
		if _, err := hex.DecodeString(s.PolicyBinding); err != nil {
			return ErrScope
		}
	}
	return nil
}

func (s Scope) Hash() string {
	s.Workspace = strings.TrimSuffix(strings.ToLower(s.Workspace), "/")
	if id, err := uuid.Parse(s.DeploymentID); err == nil {
		s.DeploymentID = id.String()
	}
	if id, err := uuid.Parse(s.DatabaseID); err == nil {
		s.DatabaseID = id.String()
	}
	s.PolicyBinding = strings.ToLower(s.PolicyBinding)
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Options struct {
	WarehouseID string
	// SourceObjects permits explicitly governed views with the same contract.
	// Every identifier is validated and quoted; no SQL expressions are allowed.
	SourceObjects  map[Stream]string
	AllowPayload   bool
	MaxRows        int
	MaxBytes       int
	QueryTimeout   time.Duration
	PollInterval   time.Duration
	Window         time.Duration
	Retention      time.Duration
	QueriesPerPoll int
	Streams        []Stream
}

func (o Options) defaults() Options {
	if o.MaxRows == 0 {
		o.MaxRows = 500
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 4 << 20
	}
	if o.QueryTimeout == 0 {
		o.QueryTimeout = 30 * time.Second
	}
	if o.PollInterval == 0 {
		o.PollInterval = time.Minute
	}
	if o.Window == 0 {
		o.Window = 15 * time.Minute
	}
	if o.Retention == 0 {
		o.Retention = SourceRetention
	}
	if o.QueriesPerPoll == 0 {
		o.QueriesPerPoll = 12
	}
	if len(o.Streams) == 0 {
		o.Streams = []Stream{Plans, Logs, ASH, DDL, DatabaseCounters, DatabaseGauges}
	}
	return o
}

func (o Options) validate() error {
	if o.WarehouseID == "" || len(o.WarehouseID) > 128 || o.MaxRows < 1 || o.MaxRows > 10000 || o.MaxBytes < 1024 || o.MaxBytes > 16<<20 || o.QueryTimeout < time.Second || o.QueryTimeout > 2*time.Minute || o.PollInterval < time.Second || o.Window < time.Millisecond || o.Window > SourceRetention || o.Retention <= 0 || o.Retention > SourceRetention || o.QueriesPerPoll < 1 || o.QueriesPerPoll > 1000 {
		return errors.New("lakebase telemetry options outside supported bounds")
	}
	seen := map[Stream]bool{}
	for _, s := range o.Streams {
		if _, ok := specs[s]; !ok || seen[s] {
			return ErrSchema
		}
		seen[s] = true
	}
	for stream, object := range o.SourceObjects {
		if _, ok := specs[stream]; !ok {
			return ErrSchema
		}
		if _, err := sourceObject(object); err != nil {
			return err
		}
	}
	return nil
}

type Window struct{ From, To time.Time }

func (w Window) Validate() error {
	if w.From.IsZero() || !w.To.After(w.From) || w.To.Sub(w.From) > SourceRetention {
		return errors.New("lakebase telemetry time window invalid")
	}
	return nil
}

// Event IDs are content identities, not guaranteed occurrence identities.
// Repeated indistinguishable provider rows collapse; exact multiplicity and
// capture completeness remain unknown until the provider supplies that contract.
type Event struct {
	ID             string            `json:"id"`
	Stream         Stream            `json:"stream"`
	ScopeHash      string            `json:"scope_hash"`
	ComputeID      string            `json:"compute_id"`
	OccurredAt     time.Time         `json:"occurred_at"`
	ReceivedAt     time.Time         `json:"received_at"`
	Version        int               `json:"version"`
	PayloadAllowed bool              `json:"payload_allowed"`
	Fields         map[string]string `json:"fields,omitempty"`
}

func NewEvent(stream Stream, scope Scope, fields map[string]string, now time.Time, payload bool) (Event, error) {
	if err := scope.Validate(); err != nil {
		return Event{}, err
	}
	if _, ok := specs[stream]; !ok {
		return Event{}, ErrSchema
	}
	if fields["project_id"] != scope.ProjectID || fields["branch_id"] != scope.BranchID || fields["endpoint_id"] != scope.EndpointID || fields["compute_id"] == "" {
		return Event{}, ErrScope
	}
	if err := checkDatabase(stream, scope, fields); err != nil {
		return Event{}, err
	}
	t, err := parseTime(fields["ts"])
	if err != nil {
		return Event{}, ErrMalformed
	}
	copyFields := make(map[string]string, len(fields))
	for k, v := range fields {
		if !payload && specs[stream].private[k] {
			v = ""
		}
		copyFields[k] = v
	}
	e := Event{Stream: stream, ScopeHash: scope.Hash(), ComputeID: fields["compute_id"], OccurredAt: t, ReceivedAt: now.UTC(), Version: SchemaVersion, PayloadAllowed: payload, Fields: copyFields}
	// Receipt time is deliberately excluded from identity.
	identity, _ := json.Marshal(struct {
		Scope  string
		Stream Stream
		Fields map[string]string
	}{e.ScopeHash, stream, copyFields})
	h := sha256.Sum256(identity)
	e.ID = hex.EncodeToString(h[:])
	return e, nil
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, ErrMalformed
}

type Batch struct {
	Events  []Event
	QueryID string
	Bytes   int
}
type Reader interface {
	ReadWindow(context.Context, Stream, Scope, Window) (Batch, error)
}
type Consumer interface {
	Name() string
	Deliver(context.Context, Event) error
}

// ConsumerFunc adapters must make sink commits idempotent by Event.ID before
// returning; a crash can occur after sink commit and before the inbox ACK.
type ConsumerFunc struct {
	ID     string
	Handle func(context.Context, Event) error
}

func (c ConsumerFunc) Name() string                               { return c.ID }
func (c ConsumerFunc) Deliver(ctx context.Context, e Event) error { return c.Handle(ctx, e) }
