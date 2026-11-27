package lakebaseobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type StreamStatus struct {
	Stream            Stream `json:"stream"`
	Cursor            Cursor `json:"cursor"`
	LastError         string `json:"last_error,omitempty"`
	HistoricalOnly    bool   `json:"historical_only"`
	Complete          bool   `json:"complete"` // No provider completeness contract exists.
	ExactMultiplicity bool   `json:"exact_multiplicity"`
	// ForwardLagSeconds is how far the forward cursor trails the last poll;
	// absent until the first forward read commits.
	ForwardLagSeconds *int64 `json:"forward_lag_seconds,omitempty"`
	// ReconcileProgress is the share (0-1) of the current deep sweep read.
	ReconcileProgress float64 `json:"reconcile_progress"`
	// Warehouse reads since CountersSince: every ReadWindow call, including
	// ErrBudget halving retries and failed reads. Rows and Bytes come from
	// the batches returned; ReadMillis is the wall time spent in reads.
	Statements    int64     `json:"statements"`
	Rows          int64     `json:"rows"`
	Bytes         int64     `json:"bytes"`
	ReadMillis    int64     `json:"read_millis"`
	CountersSince time.Time `json:"counters_since"`
}
type Runtime struct {
	reader    Reader
	store     *Store
	scope     Scope
	options   Options
	consumers []Consumer
	binding   string
	pollMu    sync.Mutex
	mu        sync.RWMutex
	statuses  map[Stream]StreamStatus
	next      int
	// narrowed holds read widths halved on ErrBudget, per stream and pass.
	// A forward width lives here until a commit persists it in the cursor;
	// a reconcile width lives here until sparse reads widen it back.
	narrowed map[narrowKey]time.Duration
	// subscribed holds each StreamConsumer's streams; absent means all.
	subscribed map[string][]Stream
	// atCommit lists, per stream, the consumers acknowledged at commit.
	atCommit map[Stream][]Consumer
	// deliveries holds each consumer's backlog from the last poll.
	deliveries map[string]DeliveryStatus
}

func NewRuntime(reader Reader, store *Store, scope Scope, options Options, consumers ...Consumer) (*Runtime, error) {
	options = options.defaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if reader == nil || store == nil {
		return nil, errors.New("lakebase telemetry reader and durable store required")
	}
	seen := map[string]bool{}
	for _, c := range consumers {
		if c == nil || !consumerName.MatchString(c.Name()) || seen[c.Name()] {
			return nil, errors.New("lakebase telemetry consumers require distinct versioned names")
		}
		seen[c.Name()] = true
	}
	for _, s := range options.Streams {
		if specs[s].compute && !scope.AllowCompute {
			return nil, ErrScope
		}
	}
	subscribed, atCommit, err := subscriptions(options.Streams, consumers)
	if err != nil {
		return nil, err
	}
	mode := "metadata"
	if options.AllowPayload {
		mode = "payload"
	}
	policy, _ := json.Marshal(options.SourceObjects)
	policyHash := sha256.Sum256(policy)
	binding := fmt.Sprintf("%s:v%d:%s:%s", scope.Hash(), SchemaVersion, mode,
		hex.EncodeToString(policyHash[:8]))
	r := &Runtime{reader: reader, store: store, scope: scope, options: options,
		consumers: consumers, binding: binding, statuses: map[Stream]StreamStatus{},
		narrowed: map[narrowKey]time.Duration{}, subscribed: subscribed, atCommit: atCommit,
		deliveries: map[string]DeliveryStatus{}}
	started := time.Now().UTC()
	for _, s := range options.Streams {
		r.statuses[s] = StreamStatus{Stream: s, HistoricalOnly: true, CountersSince: started}
	}
	return r, nil
}
func (r *Runtime) Binding() string { return r.binding }
func (r *Runtime) Scope() Scope    { return r.scope }
func (r *Runtime) Status() []StreamStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]StreamStatus, 0, len(r.options.Streams))
	for _, s := range r.options.Streams {
		out = append(out, r.statuses[s])
	}
	return out
}

func (r *Runtime) Poll(ctx context.Context, now time.Time) error {
	r.pollMu.Lock()
	defer r.pollMu.Unlock()
	now = now.UTC()
	if err := r.store.BindOwner(ctx, r.scope); err != nil {
		return err
	}
	if err := r.store.checkPolicy(ctx, r.binding, r.options.AllowPayload); err != nil {
		return err
	}
	if err := r.store.EnforceRetention(ctx, r.scope.Hash(), r.options.Retention); err != nil {
		return err
	}
	errs := r.collect(ctx, r.store, now)
	errs = append(errs, r.deliver(ctx, now)...)
	if err := r.store.Purge(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *Runtime) Run(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	for {
		if err := r.Poll(ctx, time.Now()); err != nil && ctx.Err() == nil && report != nil {
			report(errors.New(safeError(err)))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func safeError(err error) string {
	switch {
	case errors.Is(err, ErrScope):
		return ErrScope.Error()
	case errors.Is(err, ErrBudget):
		return ErrBudget.Error()
	case errors.Is(err, ErrSchema):
		return ErrSchema.Error()
	case errors.Is(err, ErrConflict):
		return ErrConflict.Error()
	case errors.Is(err, ErrPolicy):
		return ErrPolicy.Error()
	case errors.Is(err, ErrStalePolicy):
		return ErrStalePolicy.Error()
	case errors.Is(err, context.Canceled):
		return "lakebase telemetry canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "lakebase telemetry timed out"
	default:
		return "lakebase telemetry collection or delivery unavailable"
	}
}
