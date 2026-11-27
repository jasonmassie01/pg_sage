package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
)

// Fleet learning runtime (roadmap phase 3). fleetLearning is nil when
// fleet learning is off or there is no control database; the fleet's
// elector is currentFleetLeader (fleet_leader.go).
var fleetLearning *fleetlearn.Service

// needInterval is how often the fleet LLM budget is re-split by need.
const needInterval = 5 * time.Minute

// firstCycleDelay lets the fleet's databases connect and register before
// the first fingerprint cycle.
const firstCycleDelay = 2 * time.Minute

// afterDelay runs fn after d unless ctx ends first.
func afterDelay(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
		fn()
	}
}

// fleetLearningScope names the fleet a control database serves: every
// sidecar of the same fleet computes the same scope, different fleets
// different ones. YAML fleets hash their database names (never stored).
// It is computed once, at startup, where each elector takes its lease; a
// later call would read a config a reload may have changed, so fenced jobs
// take the scope from the elector's Fence token instead.
func fleetLearningScope(c *config.Config) string {
	switch {
	case c == nil:
		return ""
	case c.HasMetaDB():
		return "meta"
	case c.IsFleet():
		names := make([]string, 0, len(c.Databases))
		for _, d := range c.Databases {
			names = append(names, d.Name)
		}
		sort.Strings(names)
		sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
		return "fleet:" + hex.EncodeToString(sum[:])[:16]
	}
	return "standalone:" + c.Postgres.Database
}

// followerNoted records the fleet-wide jobs whose first skip as a
// follower was logged (once per job per process).
var followerNoted sync.Map

// fleetLeaderAllows reports whether this sidecar may run the fleet-wide
// job now; a follower skips it.
func fleetLeaderAllows(job string) bool {
	if currentFleetLeader().IsLeader() {
		return true
	}
	if _, seen := followerNoted.LoadOrStore(job, true); !seen {
		logInfo("leader", "%s runs on the leader sidecar of this fleet, not here "+
			"(see /api/v1/fleet/leader)", job)
	}
	return false
}

// fleetLearningSources lists the fleet's connected databases with their
// sharing boundary. Tags are read through InstanceConfigs, under the
// manager lock, because a meta-DB tag edit swaps Config in place.
func fleetLearningSources(mgr *fleet.DatabaseManager) func() []fleetlearn.DatabaseSource {
	return func() []fleetlearn.DatabaseSource {
		if mgr == nil {
			return nil
		}
		configs := mgr.InstanceConfigs()
		var out []fleetlearn.DatabaseSource
		for name, inst := range mgr.Instances() {
			cfg, ok := configs[name]
			if inst == nil || inst.Pool == nil || !ok {
				continue
			}
			out = append(out, fleetlearn.DatabaseSource{Name: name, Pool: inst.Pool,
				Boundary: fleetlearn.Boundary(cfg.Tags)})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
}

// startFleetLearning starts leader election, the fingerprint cycle and the
// need-based budget split on the control database.
func startFleetLearning(ctx context.Context, control *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	if cfg == nil || control == nil {
		return
	}
	fl := cfg.FleetLearning
	scope := fleetLearningScope(cfg)
	if fl.LeaderLeaseSeconds > 0 {
		setFleetLeader(startFleetElector(ctx, control, mgr, scope)) // fleet_control.go
	}
	if fl.Enabled {
		fleetLearning = fleetlearn.NewService(fleetlearn.NewStore(control, scope),
			fleetLearningSources(mgr), fleetlearn.Settings{IncludeNames: fl.IncludeNames,
				MinSimilarity: fl.LookalikeMinSimilarity, MinPriorOutcomes: fl.MinPriorOutcomes,
				OrphanScopeAge: fl.OrphanScopeAge()},
			logStructuredWrapper)
		svc := fleetLearning
		go afterDelay(ctx, firstCycleDelay, func() {
			go watchFleetLearningPeers(ctx, peerCheckPeriod, svc.Eligible,
				runFleetLearningCycle)
			every(ctx, fl.Interval(), runFleetLearningCycle)
		})
	}
	if fleetLLMBudget != nil {
		go every(ctx, needInterval, func(c context.Context) {
			applyFleetBudgetSplit(measureFleetNeeds(c, mgr))
		})
	}
	logInfo("fleet", "fleet learning: scope=%s learning=%v election=%v", scope,
		fl.Enabled, fl.LeaderLeaseSeconds > 0)
}

// fleetLearningCycleMu serializes the interval loop and the peer watcher.
var fleetLearningCycleMu sync.Mutex

// runFleetLearningCycle fingerprints the fleet when this sidecar leads,
// fencing every write with the lease it holds. A cycle skipped for want of
// a peer is logged by the service, once per entry into that state.
func runFleetLearningCycle(ctx context.Context) {
	svc := fleetLearning
	if svc == nil || !fleetLeaderAllows("fleet learning") {
		return
	}
	fleetLearningCycleMu.Lock()
	defer fleetLearningCycleMu.Unlock()
	fence := fleetlearn.Fence{}
	if el := currentFleetLeader(); el != nil {
		tok, ok := el.Fence()
		if !ok {
			return
		}
		fence = fleetlearn.Fence{Holder: tok.Holder, Epoch: tok.Epoch}
	}
	res, err := svc.RunCycle(ctx, fence)
	if errors.Is(err, fleetlearn.ErrFenced) {
		logWarn("fleet", "fleet learning stopped: the leader lease moved to "+
			"another sidecar mid-cycle")
		return
	}
	if err != nil {
		logWarn("fleet", "fleet learning cycle failed: %v", err)
		return
	}
	if res.Skipped != "" {
		return
	}
	logInfo("fleet", "fleet learning: fingerprinted %d databases (%d skipped)",
		res.Databases, len(res.Failed))
}

const activeIncidentsSQL = `/* pg_sage */ SELECT count(*) FROM sage.incidents
	WHERE resolved_at IS NULL`

// measureFleetNeeds weighs every database by its open and critical
// findings (cached status) and unresolved incidents. A database whose
// incidents cannot be read is weighed by its findings alone.
func measureFleetNeeds(ctx context.Context, mgr *fleet.DatabaseManager) map[string]float64 {
	out := map[string]float64{}
	if mgr == nil {
		return out
	}
	for name, inst := range mgr.Instances() {
		if inst == nil {
			continue
		}
		need := fleetlearn.Need{}
		if s := inst.SnapshotStatus(); s != nil {
			need.OpenFindings, need.CriticalFindings = s.FindingsOpen, s.FindingsCritical
		}
		if inst.Pool != nil {
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := inst.Pool.QueryRow(qctx, activeIncidentsSQL).
				Scan(&need.ActiveIncidents); err != nil {
				logWarn("fleet", "budget need of %s: incidents unreadable, "+
					"using findings only: %v", name, err)
				need.ActiveIncidents = 0
			}
			cancel()
		}
		out[name] = fleetlearn.NeedWeight(need)
	}
	return out
}

// applyFleetBudgetSplit applies the configured split and the measured
// needs to the fleet LLM budget (nil: per-database budgeting is off).
func applyFleetBudgetSplit(weights map[string]float64) {
	b := fleetLLMBudget
	if b == nil || cfg == nil {
		return
	}
	fl := cfg.FleetLearning
	b.SetSplit(fl.BudgetSplit, fl.BudgetFloorPct, fl.BudgetCeilingPct)
	b.SetNeeds(weights)
}

// peerCheckPeriod is how often the fleet's databases are checked (in
// memory, no SQL) for a newly possible look-alike.
const peerCheckPeriod = time.Minute

// watchFleetLearningPeers runs run at once when eligible flips to true,
// for example when a second database registers, instead of waiting up to
// the learning interval. Steady state stays with the interval loop.
func watchFleetLearningPeers(ctx context.Context, period time.Duration,
	eligible func() bool, run func(context.Context)) {
	was := eligible()
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := eligible()
			if now && !was {
				run(ctx)
			}
			was = now
		}
	}
}
