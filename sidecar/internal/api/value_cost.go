package api

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/value"
)

// market-04: GET /api/v1/value carries a cost section next to the toil
// hours. Dollars come only from cost.rates and always carry their source
// and date; without rates the section says so and shows no figure.

const (
	// costDefaultWindow is the cost window without since/until.
	costDefaultWindow = 7 * 24 * time.Hour
	// costMaxWindow is the longest window priced (the host-usage retention).
	costMaxWindow   = cost.UsageRetention
	costReadTimeout = 15 * time.Second
	// reasonCostUnwired: the value reader has no cost attribution.
	reasonCostUnwired = "cost attribution is not available on this server"
)

// costReporter is a value reader that also prices the instances.
type costReporter interface {
	Cost(context.Context, value.Filter) cost.Section
}

// valueResponse is the value report and its cost section.
type valueResponse struct {
	value.Report
	Cost cost.Section `json:"cost"`
}

// fleetValue reads the fleet's value ledgers and prices its instances.
type fleetValue struct {
	*value.FleetService
	mgr   *fleet.DatabaseManager
	rates cost.RateSource
	now   func() time.Time
}

// Cost implements costReporter.
func (f *fleetValue) Cost(ctx context.Context, filter value.Filter) cost.Section {
	ctx, cancel := context.WithTimeout(ctx, costReadTimeout)
	defer cancel()
	from, to := costWindow(filter, f.now())
	members := costMembers(fleet.ValueSources(f.mgr), filter.Database)
	return cost.Build(ctx, cost.PGReader{}, members, f.rates, from, to)
}

func newFleetValue(mgr *fleet.DatabaseManager, cfg *config.Config) *fleetValue {
	var rates cost.RateSource
	if cfg != nil {
		rates = cost.NewConfigRates(cfg.Cost)
	}
	return &fleetValue{FleetService: value.NewFleetService(func() []value.Source {
		return fleet.ValueSources(mgr)
	}), mgr: mgr, rates: rates, now: time.Now}
}

// costWindow is the filter's window, defaulting to the last 7 days and
// never longer than the host-usage retention or past now.
func costWindow(filter value.Filter, now time.Time) (time.Time, time.Time) {
	to := now
	if !filter.Until.IsZero() && filter.Until.Before(now) {
		to = filter.Until
	}
	from := to.Add(-costDefaultWindow)
	if !filter.Since.IsZero() {
		from = filter.Since
	}
	if to.Sub(from) > costMaxWindow {
		from = to.Add(-costMaxWindow)
	}
	if from.After(to) {
		from = to
	}
	return from, to
}

// costMembers are the selected databases and the provider resource each
// runs on ("" when no cloud telemetry names it).
func costMembers(sources []value.Source, database string) []cost.Member {
	out := make([]cost.Member, 0, len(sources))
	for _, s := range sources {
		if database != "" && database != "all" && s.Name != database {
			continue
		}
		m := cost.Member{Database: s.Name, Pool: s.Pool}
		if rt := cloudtel.Lookup(s.Name); rt != nil {
			m.Resource = rt.Resource()
		}
		out = append(out, m)
	}
	return out
}

// costSection prices the report when the reader can.
func costSection(ctx context.Context, reader valueReader, filter value.Filter) cost.Section {
	if c, ok := reader.(costReporter); ok {
		return c.Cost(ctx, filter)
	}
	return cost.Section{Reason: reasonCostUnwired, TopQueries: []cost.QueryCost{},
		Resources: []cost.ResourceCost{}}
}
