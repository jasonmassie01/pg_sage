package main

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// cloudProviderEvents reads the managed-cloud provider's maintenance and
// failover events (RDS DescribeEvents, Cloud SQL operations) into the
// change feed's provider_api source (guard-02b).
type cloudProviderEvents struct{ rt *cloudtel.Runtime }

// providerEventKinds maps cloud telemetry's categories to feed kinds.
var providerEventKinds = map[string]changefeed.Kind{
	cloudtel.EventRestart:       changefeed.KindRestart,
	cloudtel.EventFailover:      changefeed.KindFailover,
	cloudtel.EventVersionChange: changefeed.KindVersionChange,
}

func (c cloudProviderEvents) ProviderEvents(ctx context.Context,
	since time.Time) ([]changefeed.ProviderEvent, error) {
	read, err := c.rt.ProviderEvents(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]changefeed.ProviderEvent, 0, len(read))
	for _, e := range read {
		kind, ok := providerEventKinds[e.Category]
		if !ok {
			continue
		}
		out = append(out, changefeed.ProviderEvent{ID: e.ID, Kind: kind, Summary: e.Message,
			At: e.At})
	}
	return out, nil
}

// providerEventReader is the database's provider event reader, or nil
// when its cloud telemetry has none (no telemetry, unavailable, or a
// provider without an event API).
func (rt *databaseRuntime) providerEventReader() changefeed.ProviderEventReader {
	return providerEventReaderFor(rt.cloud)
}

func providerEventReaderFor(rt *cloudtel.Runtime) changefeed.ProviderEventReader {
	if !rt.HasProviderEvents() {
		return nil
	}
	return cloudProviderEvents{rt: rt}
}
