package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/store"
)

// observedSweep is member's newest sweep when it is observe-only
// (fleetpol-01d); observe is false for a full member.
func (g metaDiscoveryGrades) observedSweep(ctx context.Context, member string) (
	*store.ObserveSweep, bool, error) {
	if g.observed == nil {
		return nil, false, nil
	}
	sw, observe, err := g.observed(ctx, member)
	if err != nil {
		return nil, false, fmt.Errorf("reading the observe-only sweep of %s: %w", member, err)
	}
	return sw, observe, nil
}

// observeGrades is an observe-only member's grade: its newest successful
// sweep's dimension letters, or none before one.
func observeGrades(sw *store.ObserveSweep, err error) ([]discovery.Grade, error) {
	if err != nil || sw == nil || sw.Status != store.ObserveOK {
		return nil, err
	}
	var d fleet.ObserveGradeDetail
	if err := json.Unmarshal(sw.Grade, &d); err != nil {
		return nil, fmt.Errorf("reading the stored grade of database %d: %w",
			sw.DatabaseID, err)
	}
	if len(d.Dimensions) == 0 {
		return nil, nil
	}
	return []discovery.Grade{{At: sw.SweptAt.UTC(), Letters: d.Dimensions}}, nil
}
