package changefeed

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// ProviderEvent is one maintenance or failover event read from the cloud
// provider's own API (RDS DescribeEvents, Cloud SQL operations).
type ProviderEvent struct {
	// ID is the provider's identity of the event, stable across reads.
	ID string
	// Kind is KindRestart, KindFailover or KindVersionChange; other kinds
	// are dropped.
	Kind    Kind
	Summary string
	At      time.Time
}

// ProviderEventReader reads the provider's events of one database
// resource that occurred at or after since.
type ProviderEventReader interface {
	ProviderEvents(ctx context.Context, since time.Time) ([]ProviderEvent, error)
}

// providerAPIFirstLook is how far back the first read reaches: the
// provider keeps its events for days, and the last day is incident
// context worth having on the timeline at once.
const providerAPIFirstLook = 24 * time.Hour

// providerAPITimeout bounds one provider API read (its own retries
// included); the catalog sources keep sourceTimeout.
const providerAPITimeout = 20 * time.Second

type providerAPIState struct {
	Since string `json:"since"`
}

// pollProviderAPI reads the provider's events after the last one seen.
// Event ids are the provider's, so a re-read records nothing twice.
func (p *Poller) pollProviderAPI(ctx context.Context, _ querier, prev []byte,
	now time.Time) ([]Event, any, error) {
	since := now.Add(-providerAPIFirstLook)
	if prev != nil {
		var st providerAPIState
		if err := json.Unmarshal(prev, &st); err != nil {
			return nil, nil, fmt.Errorf("provider_api state: %w", err)
		}
		since = parseUTC(st.Since, since)
	}
	read, err := p.providerAPI.ProviderEvents(ctx, since)
	if err != nil {
		return nil, nil, err
	}
	latest := since
	var events []Event
	for _, pe := range read {
		if !attributable(pe.Kind) || pe.ID == "" || pe.At.Before(since) {
			continue
		}
		e := internalEvent(pe.Kind, pe.ID, pe.Summary, pe.At, now)
		e.Source = SourceProviderAPI
		e.Hash = hashOf(e)
		events = append(events, e)
		if pe.At.After(latest) {
			latest = pe.At
		}
	}
	return events, providerAPIState{Since: latest.UTC().Format(time.RFC3339Nano)}, nil
}
