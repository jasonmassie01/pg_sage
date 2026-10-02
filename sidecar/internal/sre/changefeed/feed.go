package changefeed

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// ScopeFunc resolves the bound database scope (the investigator's).
type ScopeFunc func(ctx context.Context) (sre.Scope, error)

// Feed is one database's view of the change feed.
type Feed struct {
	store *Store
	name  string
	scope ScopeFunc
}

// NewFeed binds a store to one database.
func NewFeed(store *Store, name string, scope ScopeFunc) *Feed {
	return &Feed{store: store, name: name, scope: scope}
}

// Name is the database's display name.
func (f *Feed) Name() string { return f.name }

// Store is the feed's store.
func (f *Feed) Store() *Store { return f.store }

func (f *Feed) bound(ctx context.Context) (sre.Scope, error) {
	if f == nil || f.store == nil || f.scope == nil {
		return sre.Scope{}, fmt.Errorf("%w: change feed is not configured",
			sre.ErrMetadataUnavailable)
	}
	return f.scope(ctx)
}

// Ingest validates and records a signed submission received at now. A
// submission naming a database must name this one; one without a
// database is deployment-wide.
func (f *Feed) Ingest(ctx context.Context, sub Submission, now time.Time) (Event, bool, error) {
	if sub.Database != "" && sub.Database != f.name {
		return Event{}, false, fmt.Errorf("%w: event for %q sent to %q", ErrInvalid,
			sub.Database, f.name)
	}
	e, err := sub.Event(now)
	if err != nil {
		return Event{}, false, err
	}
	scope, err := f.bound(ctx)
	if err != nil {
		return Event{}, false, err
	}
	return f.store.Record(ctx, scope, sub.Database != "", e)
}

// Recent lists the changes of the last window, newest first.
func (f *Feed) Recent(ctx context.Context, window time.Duration, limit int) ([]Event, error) {
	scope, err := f.bound(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return f.store.List(ctx, scope, Filter{Since: now.Add(-window), Until: now.Add(maxFuture),
		Limit: limit})
}

// probeLimit bounds the changes one change_feed probe returns.
const probeLimit = 50

// Probe is the change_feed signal probe: the changes in the window
// (default 24 h) as typed evidence. A feed that cannot be read is an
// error result, never an empty (healthy) one.
func (f *Feed) Probe(ctx context.Context, args probes.Args) probes.Result {
	res := probes.Result{ProbeID: probes.ChangeFeed, Version: "v1", ObservedAt: time.Now()}
	window := args.Window
	if window <= 0 {
		window = probes.DefaultWindow
	}
	start := time.Now()
	events, err := f.Recent(ctx, window, probeLimit)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Status, res.Reason, res.Error = probes.StatusError, "store_unavailable",
			truncate(err.Error(), 200)
		return res
	}
	res.Columns = []string{"kind", "source", "service", "summary", "occurred_at", "age_s",
		"signature", "event_id"}
	for _, e := range events {
		res.Rows = append(res.Rows, probes.Row{"kind": string(e.Kind), "source": e.Source,
			"service": e.Service, "summary": e.Summary, "occurred_at": e.OccurredAt,
			"age_s": res.ObservedAt.Sub(e.OccurredAt).Seconds(), "signature": e.Signature,
			"event_id": e.EventID})
	}
	res.Status = probes.StatusOK
	if len(res.Rows) == 0 {
		res.Status, res.Reason = probes.StatusEmpty, "no_rows"
	}
	return res
}
