package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// errFleetRuntimeCandidate refuses a reload whose new runtime for a
// running database could not be built: the running runtime keeps going.
var errFleetRuntimeCandidate = errors.New("fleet database runtime could not be prepared")

// fleetDatabasesOwner is the "fleet_databases" reconfiguration owner of a
// YAML fleet: a reload adds, removes and rebuilds per-database runtimes
// and applies per-database policy in place, without a restart.
type fleetDatabasesOwner struct {
	manager     func() *fleet.DatabaseManager
	controlName string
	controlPool *pgxpool.Pool
}

// registerFleetDatabasesOwner makes YAML fleet databases reloadable. The
// control database is fixed at startup.
func registerFleetDatabasesOwner(boot *fleetBootstrap) {
	if configController == nil || boot == nil {
		return
	}
	owner := &fleetDatabasesOwner{
		manager:     func() *fleet.DatabaseManager { return fleetMgr },
		controlName: boot.controlName, controlPool: boot.controlPool,
	}
	if err := configController.RegisterOwner(owner); err != nil {
		logWarn("config", "fleet database reconfiguration owner: %v", err)
	}
}

func (*fleetDatabasesOwner) Name() string { return config.FleetDatabasesOwner }

// Prepare builds every new runtime without publishing it. A runtime that
// cannot be built for a running database rejects the whole reload; an
// added or currently failed database is published as failed instead, as
// at startup.
func (o *fleetDatabasesOwner) Prepare(
	ctx context.Context, active, desired config.ConfigSnapshot,
) (config.PreparedReconfiguration, error) {
	plan := planFleetReload(active.Config.Databases, desired.Config.Databases)
	if err := validateFleetReloadPlan(plan, o.controlName); err != nil {
		return nil, err
	}
	mgr := o.manager()
	if mgr == nil {
		return nil, fmt.Errorf("fleet manager is unavailable")
	}
	p := &preparedFleetReload{
		mgr: mgr, plan: plan,
		next:     fleetDatabaseSettings{active: desired.Config},
		previous: fleetDatabaseSettings{active: active.Config},
	}
	p.observe()
	if err := o.prepareCandidates(ctx, p); err != nil {
		return nil, errors.Join(err, p.discardCandidates())
	}
	logFleetReloadPlan(plan)
	return p, nil
}

// fleetCandidate is a new runtime (or failed placeholder) for an added or
// rebuilt database, and the generation it replaces (nil for an addition).
type fleetCandidate struct {
	cfg      config.DatabaseConfig
	old      *fleet.DatabaseInstance
	instance *fleet.DatabaseInstance
}

// fleetHotChange applies per-database policy to a running generation.
type fleetHotChange struct {
	instance *fleet.DatabaseInstance
	old, cfg config.DatabaseConfig
}

// fleetDatabaseSettings is the process config a reload publishes or
// restores.
type fleetDatabaseSettings struct {
	active *config.Config
}

func (s fleetDatabaseSettings) publish() {
	if s.active == nil || cfg == nil {
		return
	}
	config.LockForHotReload()
	defer config.UnlockForHotReload()
	cfg.Databases = append([]config.DatabaseConfig(nil), s.active.Databases...)
	cfg.Defaults = s.active.Defaults
}

type preparedFleetReload struct {
	mgr        *fleet.DatabaseManager
	plan       fleetReloadPlan
	next       fleetDatabaseSettings
	previous   fleetDatabaseSettings
	candidates []fleetCandidate
	removals   []*fleet.DatabaseInstance
	hot        []fleetHotChange
	committed  bool
}

// observe records the generation each change applies to. An added name
// that is already registered (a stale placeholder) is replaced.
func (p *preparedFleetReload) observe() {
	for _, db := range p.plan.Add {
		p.candidates = append(p.candidates,
			fleetCandidate{cfg: db, old: p.mgr.GetInstance(db.Name)})
	}
	for _, db := range p.plan.Rebuild {
		p.candidates = append(p.candidates,
			fleetCandidate{cfg: db, old: p.mgr.GetInstance(db.Name)})
	}
	for _, name := range p.plan.Remove {
		if inst := p.mgr.GetInstance(name); inst != nil {
			p.removals = append(p.removals, inst)
		}
	}
	for _, db := range p.plan.Hot {
		if inst := p.mgr.GetInstance(db.Name); inst != nil {
			p.hot = append(p.hot, fleetHotChange{
				instance: inst, old: p.plan.Previous[db.Name], cfg: db,
			})
		}
	}
}

func (o *fleetDatabasesOwner) prepareCandidates(
	ctx context.Context, p *preparedFleetReload,
) error {
	for i := range p.candidates {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("prepare fleet databases: %w", err)
		}
		candidate := &p.candidates[i]
		instance, err := o.prepareCandidate(ctx, candidate.cfg, candidate.old)
		if err != nil {
			return err
		}
		candidate.instance = instance
	}
	return nil
}

func (o *fleetDatabasesOwner) prepareCandidate(
	ctx context.Context, db config.DatabaseConfig, old *fleet.DatabaseInstance,
) (*fleet.DatabaseInstance, error) {
	rt, err := prepareFleetRuntime(db, o.controlPool)
	if err != nil {
		if old != nil && old.Pool != nil {
			return nil, fmt.Errorf("%w: db %q: %w; keeping the running runtime",
				errFleetRuntimeCandidate, db.Name, err)
		}
		return failedFleetInstance(db, err), nil
	}
	instance := rt.inst
	instance.DatabaseID = o.databaseID(ctx, db, old)
	return instance, nil
}

// databaseID keeps a rebuilt database's sage.databases id and registers an
// added one on the control database, as startup does.
func (o *fleetDatabasesOwner) databaseID(
	ctx context.Context, db config.DatabaseConfig, old *fleet.DatabaseInstance,
) int {
	if old != nil && old.DatabaseID > 0 {
		return old.DatabaseID
	}
	if o.controlPool == nil {
		return 0
	}
	upsertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, err := upsertFleetDatabase(upsertCtx, o.controlPool, db, db.TrustLevel)
	if err != nil {
		logWarn("fleet", "db %q: register in sage.databases: %v", db.Name, err)
		return 0
	}
	return id
}

func logFleetReloadPlan(plan fleetReloadPlan) {
	if plan.Empty() {
		return
	}
	logInfo("fleet", "reload plan: add=%v remove=%v rebuild=%v in_place=%v",
		configNames(plan.Add), plan.Remove, configNames(plan.Rebuild),
		configNames(plan.Hot))
}

func configNames(dbs []config.DatabaseConfig) []string {
	names := make([]string, 0, len(dbs))
	for _, db := range dbs {
		names = append(names, db.Name)
	}
	return names
}
