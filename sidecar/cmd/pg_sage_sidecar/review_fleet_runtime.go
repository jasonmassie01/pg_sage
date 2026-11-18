package main

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/cost"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/fleetlearn"
	"github.com/pg-sage/sidecar/internal/llm"
	reviewfleet "github.com/pg-sage/sidecar/internal/review/fleet"
)

// Sage Review's fleet tiering (review-04b): wiring. In fleet and meta mode,
// with review.enabled, the leader runs one tiered pass of the fleet a
// calendar month on the control database (sage.review_fleet_*), and
// between passes advances it: members whose deep review ran become
// reviewed, and members a stop or a refused trigger left waiting are
// scheduled. A deep review is a fleet_outlier trigger in the member's own
// queue, which its review trigger loop runs. Single-database installs
// never start it.

// The loop's timing; tests shorten them.
var (
	reviewFleetFirst = 10 * time.Minute
	reviewFleetTick  = time.Hour
)

// reviewFleetWorkers is how many members' rollups are read at once.
const reviewFleetWorkers = 8

// reviewFleetOn reports whether c runs the fleet tiers: fleet or meta mode
// with review enabled.
func reviewFleetOn(c *config.Config) bool {
	return c != nil && c.Review.Enabled && (c.IsFleet() || c.HasMetaDB())
}

// reviewTriageModel is review.triage_model ("" without a config).
func reviewTriageModel() string {
	if cfg == nil {
		return ""
	}
	return cfg.Review.TriageModel
}

// startReviewFleet starts the fleet tiering loop on the control database.
func startReviewFleet(ctx context.Context, control *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	if !reviewFleetOn(cfg) || control == nil || mgr == nil {
		return
	}
	scope := fleetLearningScope(cfg)
	loop := &reviewFleetLoop{store: reviewfleet.NewStore(control, scope),
		learn: fleetlearn.NewStore(control, scope), members: fleetMemberPools(mgr),
		stopped: mgr.FleetStopped, settings: reviewFleetSettings,
		triage: reviewFleetTriageModel, review: reviewFleetSummaryModel,
		rates: func() cost.RateSource { return cost.NewConfigRates(cfg.Cost) }}
	go afterDelay(ctx, reviewFleetFirst, func() {
		every(ctx, reviewFleetTick, func(c context.Context) {
			if !fleetLeaderAllows("fleet review") {
				return
			}
			loop.logTick(loop.tick(c))
		})
	})
	logInfo("fleet", "review fleet tiering: scope=%s fleet_deep_max=%d", scope,
		cfg.Review.FleetDeepMax)
}

// fleetMemberPools lists the fleet's connected members by name.
func fleetMemberPools(mgr *fleet.DatabaseManager) func() map[string]*pgxpool.Pool {
	return func() map[string]*pgxpool.Pool {
		out := map[string]*pgxpool.Pool{}
		for name, inst := range mgr.Instances() {
			if inst != nil && inst.Pool != nil {
				out[name] = inst.Pool
			}
		}
		return out
	}
}

func reviewFleetSettings() reviewfleet.Settings {
	return reviewfleet.Settings{DeepMax: cfg.Review.FleetDeepMax, Workers: reviewFleetWorkers,
		Price: llm.PricingFrom(cfg.LLM.Pricing)}
}

var (
	reviewTriageOnce   sync.Once
	reviewTriageClient *llm.Client
)

// reviewFleetTriageModel is the triage tier's model: none with llm
// disabled, the general client without review.triage_model, else a
// registered client of the triage role.
func reviewFleetTriageModel() reviewfleet.Model {
	if !cfg.LLM.Enabled {
		return nil
	}
	if reviewTriageModel() == "" {
		return reviewFleetSummaryModel()
	}
	reviewTriageOnce.Do(func() {
		reviewTriageClient = llmClients.newClient(llmRoleTriage, "fleet", true)
	})
	return reviewTriageClient
}

// reviewFleetSummaryModel is the fleet review's model: the general client.
func reviewFleetSummaryModel() reviewfleet.Model {
	if !cfg.LLM.Enabled || llmClient == nil {
		return nil
	}
	return llmClient
}

// reviewFleetLoop is one fleet's tiering loop.
type reviewFleetLoop struct {
	store    *reviewfleet.Store
	learn    *fleetlearn.Store
	members  func() map[string]*pgxpool.Pool
	stopped  func() bool
	settings func() reviewfleet.Settings
	triage   func() reviewfleet.Model
	review   func() reviewfleet.Model
	rates    func() cost.RateSource
	now      func() time.Time
}

// tick runs this month's pass when none finished yet (a failed one is
// retried), else advances the fleet.
func (l *reviewFleetLoop) tick(ctx context.Context) (reviewfleet.Result, error) {
	runner := l.runner(ctx)
	status, ran, err := l.store.LastPass(ctx, reviewfleet.Period(runner.Now()))
	if err != nil {
		return reviewfleet.Result{}, err
	}
	if ran && status != reviewfleet.PassFailed {
		return runner.Advance(ctx)
	}
	return runner.Run(ctx, poolNames(l.members()))
}

func (l *reviewFleetLoop) runner(ctx context.Context) *reviewfleet.Runner {
	pools := l.members()
	pick := func(db string) *pgxpool.Pool { return pools[db] }
	now := l.now
	if now == nil {
		now = time.Now
	}
	src := &reviewfleet.PoolSource{Pool: pick, Fingerprints: l.schemaKeys(ctx), Now: now}
	if l.rates != nil {
		src.Rates = l.rates()
	}
	r := &reviewfleet.Runner{Ledger: l.store, Source: src,
		Deep: &reviewfleet.TriggerScheduler{Pool: pick}, Stopped: l.stopped, Now: now}
	if l.settings != nil {
		r.Settings = l.settings()
	}
	if l.triage != nil {
		r.Triage = l.triage()
	}
	if l.review != nil {
		r.Review = l.review()
	}
	return r
}

// schemaKeys are the members' schema-shape keys from fleet learning; none
// when fingerprints are unreadable (shared anti-patterns are then not
// grouped, never guessed).
func (l *reviewFleetLoop) schemaKeys(ctx context.Context) map[string]string {
	if l.learn == nil {
		return nil
	}
	fps, err := l.learn.Fingerprints(ctx)
	if err != nil {
		logWarn("fleet", "review fleet: fingerprints unreadable, shared anti-patterns "+
			"are not grouped: %v", err)
		return nil
	}
	return reviewfleet.SchemaKeys(fps)
}

func (l *reviewFleetLoop) logTick(res reviewfleet.Result, err error) {
	if err != nil {
		logWarn("fleet", "review fleet pass for %s failed: %v", res.Period, err)
		return
	}
	if res.PassID == 0 && len(res.Scheduled) == 0 && res.Reviewed == 0 {
		return
	}
	logInfo("fleet", "review fleet %s: pass %d, %d members, %d outliers, %d deep reviews "+
		"scheduled, %d waiting, %d reviewed, halted=%v, triage=%s", res.Period, res.PassID,
		res.Members, res.Outliers, len(res.Scheduled), len(res.Deferred), res.Reviewed,
		res.Halted, res.Triage.Mode)
}

func poolNames(pools map[string]*pgxpool.Pool) []string {
	out := make([]string, 0, len(pools))
	for name := range pools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
