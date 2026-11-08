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
	mode := "metadata"
	if options.AllowPayload {
		mode = "payload"
	}
	policy, _ := json.Marshal(options.SourceObjects)
	policyHash := sha256.Sum256(policy)
	r := &Runtime{reader: reader, store: store, scope: scope, options: options, consumers: consumers, binding: fmt.Sprintf("%s:v%d:%s:%s", scope.Hash(), SchemaVersion, mode, hex.EncodeToString(policyHash[:8])), statuses: map[Stream]StreamStatus{}}
	for _, s := range options.Streams {
		r.statuses[s] = StreamStatus{Stream: s, HistoricalOnly: true}
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
	remaining := r.options.QueriesPerPoll
	if err := r.store.BindOwner(ctx, r.scope); err != nil {
		return err
	}
	if err := r.store.checkPolicy(ctx, r.binding, r.options.AllowPayload); err != nil {
		return err
	}
	if err := r.store.EnforceRetention(ctx, r.scope.Hash(), r.options.Retention); err != nil {
		return err
	}
	var errs []error
	// Rotate priority so one permanently saturated source cannot starve others.
	for i := 0; i < len(r.options.Streams) && remaining > 0; i++ {
		stream := r.options.Streams[(r.next+i)%len(r.options.Streams)]
		err := r.pollStream(ctx, stream, now, &remaining)
		r.mu.Lock()
		st := r.statuses[stream]
		if err != nil {
			st.LastError = safeError(err)
		} else {
			st.LastError = ""
		}
		r.statuses[stream] = st
		r.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
		}
	}
	r.next = (r.next + 1) % len(r.options.Streams)
	for _, consumer := range r.consumers {
		events, err := r.store.Pending(ctx, r.binding, consumer.Name(), r.options.MaxRows, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range events {
			deliveryCtx, cancel := context.WithTimeout(ctx, r.options.QueryTimeout)
			err = r.store.DeliverAndAck(deliveryCtx, r.binding, e.ID, consumer, now)
			cancel()
			if err != nil {
				if errors.Is(err, ErrPolicy) || errors.Is(err, ErrStalePolicy) {
					errs = append(errs, err)
					break
				}
				errs = append(errs, fmt.Errorf("lakebase telemetry consumer %s failed", consumer.Name()))
				break
			}
		}
	}
	if err := r.store.Purge(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *Runtime) pollStream(ctx context.Context, stream Stream, now time.Time, remaining *int) error {
	c, err := r.store.LoadCursor(ctx, r.binding, stream)
	if err != nil {
		return err
	}
	oldest := now.Add(-SourceRetention)
	if !c.LastPoll.IsZero() && c.LastPoll.Before(oldest) {
		c.GapCount++
		c.LastGap = "source retention exceeded during outage"
	}
	for _, reconcile := range []bool{false, true} {
		if *remaining <= 0 {
			break
		}
		before := c
		from := c.ForwardThrough
		if reconcile {
			from = c.ReconcileThrough
			// Pin a sweep's upper boundary. Chasing a moving `now` forever
			// would strand reconciliation at the tail and never rotate back.
			if c.ReconcileTarget.IsZero() || !from.Before(c.ReconcileTarget) {
				from = oldest
				c.ReconcileTarget = now
			}
		}
		if from.IsZero() {
			if reconcile {
				from = oldest
			} else {
				from = now.Add(-r.options.Window)
			}
		}
		if from.Before(oldest) {
			from = oldest
			c.GapCount++
			c.LastGap = "unread history expired before reconciliation"
		}
		if !from.Before(now) {
			if reconcile {
				from = oldest
			} else {
				from = now.Add(-r.options.Window)
			}
		}
		to := from.Add(r.options.Window)
		if to.After(now) {
			to = now
		}
		if reconcile && to.After(c.ReconcileTarget) {
			to = c.ReconcileTarget
		}
		var b Batch
		for {
			if *remaining <= 0 {
				return ErrBudget
			}
			*remaining--
			b, err = r.reader.ReadWindow(ctx, stream, r.scope, Window{from, to})
			if !errors.Is(err, ErrBudget) {
				break
			}
			if to.Sub(from) <= time.Millisecond {
				return ErrBudget
			}
			to = from.Add(to.Sub(from) / 2)
		}
		if err != nil {
			return err
		}
		for _, e := range b.Events {
			if e.ScopeHash != r.scope.Hash() || e.Stream != stream || e.OccurredAt.Before(from) || !e.OccurredAt.Before(to) {
				return ErrScope
			}
		}
		if reconcile {
			c.ReconcileThrough = to
		} else if to.After(c.ForwardThrough) {
			c.ForwardThrough = to
		}
		c.LastPoll = now
		for _, e := range b.Events {
			if e.OccurredAt.After(c.LastEvent) {
				c.LastEvent = e.OccurredAt
			}
		}
		if err = r.store.CommitBatch(ctx, r.binding, stream, before, c, b.Events, r.options.Retention); err != nil {
			return err
		}
		c.Revision = before.Revision + 1
		r.mu.Lock()
		st := r.statuses[stream]
		st.Cursor = c
		r.statuses[stream] = st
		r.mu.Unlock()
	}
	return nil
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
