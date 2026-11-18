package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/fleet/discovery"
	"github.com/pg-sage/sidecar/internal/review/rubric"
	"github.com/pg-sage/sidecar/internal/review/trigger"
	"github.com/pg-sage/sidecar/internal/store"
)

// Enrollment by policy set (fleetpol-01b): wiring. Discovery reads the
// policy sets from the meta database, asks a newly enrolled member's
// review queue for its enrollment review (review-04a) and reads its
// grades (review-01a) from the member's own sage schema. /metrics reads
// the runner's last numbers, with no database I/O.

// fleetDiscoveryRunner is the running discovery runner, for /metrics.
var fleetDiscoveryRunner atomic.Pointer[discovery.Runner]

// writeFleetEnrollMetrics exports pg_sage_fleet_enroll_seconds and
// pg_sage_fleet_baseline_24h_ratio when discovery runs with policy sets.
func writeFleetEnrollMetrics(b *strings.Builder) {
	if r := fleetDiscoveryRunner.Load(); r != nil {
		r.WriteMetrics(b)
	}
}

// discoveryGradeRuns bounds the grades read per member and cycle.
const discoveryGradeRuns = 20

// memberPool is a running member's pool, or nil.
type memberPool func(member string) *pgxpool.Pool

// metaDiscoveryGrades asks for and reads discovered members' grades.
type metaDiscoveryGrades struct {
	hub  *trigger.Hub
	pool memberPool
	// observed reads an observe-only member's newest sweep (fleetpol-01d);
	// nil treats every member as full.
	observed func(ctx context.Context, member string) (*store.ObserveSweep, bool, error)
}

func newMetaDiscoveryGrades(st *store.DatabaseStore) metaDiscoveryGrades {
	g := metaDiscoveryGrades{hub: trigger.Default, pool: func(member string) *pgxpool.Pool {
		if fleetMgr == nil {
			return nil
		}
		if inst := fleetMgr.GetInstance(member); inst != nil {
			return inst.Pool
		}
		return nil
	}}
	if st != nil {
		g.observed = st.ObserveSweepByName
	}
	return g
}

// RequestFirstGrade fires the member's enrollment trigger; a member
// whose review queue is not registered yet is not ready.
func (g metaDiscoveryGrades) RequestFirstGrade(ctx context.Context, member string,
	row store.DiscoveredRow) error {
	if _, observe, err := g.observedSweep(ctx, member); err != nil || observe {
		return err // an observe-only member is graded by every sweep round
	}
	err := g.hub.Enrolled(ctx, member, map[string]any{"source": row.Source,
		"external_id": row.ExternalID, "set": row.SetName})
	if errors.Is(err, trigger.ErrUnknownDatabase) {
		return fmt.Errorf("%w: %v", discovery.ErrMemberNotReady, err)
	}
	return err
}

// Grades is the member's finished review-01 grades, oldest first.
func (g metaDiscoveryGrades) Grades(ctx context.Context, member string) (
	[]discovery.Grade, error) {
	if sw, observe, err := g.observedSweep(ctx, member); err != nil || observe {
		return observeGrades(sw, err)
	}
	pool := g.pool(member)
	if pool == nil {
		return nil, fmt.Errorf("%w: %s has no pool yet", discovery.ErrMemberNotReady, member)
	}
	runs, err := rubric.NewStore(pool).Oldest(ctx, member, discoveryGradeRuns)
	if err != nil {
		return nil, err
	}
	out := make([]discovery.Grade, 0, len(runs))
	for _, run := range runs {
		if run.FinishedAt == nil {
			continue
		}
		letters := make(map[string]string, len(run.Grade.Dimensions))
		for _, d := range run.Grade.Dimensions {
			letters[d.Dimension] = string(d.Letter)
		}
		out = append(out, discovery.Grade{At: run.FinishedAt.UTC(), Letters: letters})
	}
	return out, nil
}

// neonDiscoveryAPI is the Neon listing for neon discovery sources, with
// the clone.neon.api_key key.
func neonDiscoveryAPI() (discovery.NeonAPI, error) {
	if cfg == nil {
		return nil, errors.New("no configuration loaded")
	}
	return clone.NewNeonLister(clone.NeonListerConfig{APIKey: cfg.Clone.Neon.APIKey})
}
