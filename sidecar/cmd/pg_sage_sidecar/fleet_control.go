package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/leader"
)

// unpinnedRefusal is why a sidecar of an unpinned YAML fleet refuses
// leadership once it sees another live sidecar on the lease: the next
// sidecar to start while a database is down can take the control role in
// a different database, where neither lease sees the other (fleet-03).
const unpinnedRefusal = "this YAML fleet has no pinned control database, so " +
	"sidecars that start while a database is down can lead in different " +
	"databases at once; set fleet_learning.control_database (or use meta_db) " +
	"on every sidecar and restart them"

// unpinnedYAMLFleet reports a YAML fleet whose control database is just
// the first database to connect.
func unpinnedYAMLFleet(c *config.Config) bool {
	return c != nil && c.IsFleet() && !c.HasMetaDB() &&
		c.FleetLearning.ControlDatabase == ""
}

// fleetStartOrder puts the pinned control database first, so it becomes
// the control (and login) database whenever it connects; the others keep
// their YAML order. Without a pin the order is unchanged.
func fleetStartOrder(dbs []config.DatabaseConfig, pin string) []config.DatabaseConfig {
	out := make([]config.DatabaseConfig, 0, len(dbs))
	for _, d := range dbs {
		if pin != "" && d.Name == pin {
			out = append(out, d)
		}
	}
	for _, d := range dbs {
		if pin == "" || d.Name != pin {
			out = append(out, d)
		}
	}
	return out
}

// electionControl is the pool the leader lease lives in. With a pinned
// control database it is that database's pool, and only when that pool is
// the fleet's control pool; otherwise refusal says why this sidecar
// follows for the run instead of leading from another database.
func electionControl(c *config.Config, mgr *fleet.DatabaseManager,
	control *pgxpool.Pool) (*pgxpool.Pool, string) {
	if c == nil || c.HasMetaDB() || c.FleetLearning.ControlDatabase == "" {
		return control, ""
	}
	pin := c.FleetLearning.ControlDatabase
	if mgr != nil && control != nil {
		if inst := mgr.GetInstance(pin); inst != nil && inst.Pool == control {
			return control, ""
		}
	}
	return nil, fmt.Sprintf("fleet_learning.control_database %q was not connected "+
		"when this sidecar started, and the lease never moves to another database; "+
		"restart this sidecar once %q is reachable", pin, pin)
}

// startFleetElector starts leader election on the fleet's control pool,
// running its first tick synchronously so leader-only loops started after
// it see the result.
func startFleetElector(ctx context.Context, control *pgxpool.Pool,
	mgr *fleet.DatabaseManager, scope string) *leader.Elector {
	if unpinnedYAMLFleet(cfg) {
		logWarn("leader", "multi-sidecar HA needs a pinned control database: set "+
			"fleet_learning.control_database to one of databases: (or use meta_db); "+
			"without it, sidecars started while a database is down can each lead")
	}
	pool, refusal := electionControl(cfg, mgr, control)
	opts := []leader.Option{leader.WithLogger(logStructuredWrapper)}
	if unpinnedYAMLFleet(cfg) {
		opts = append(opts, leader.WithForeignHolderRefusal(unpinnedRefusal))
	}
	e := leader.NewElector(leader.NewPostgresStore(pool), scope, leader.HolderID(),
		cfg.FleetLearning.LeaderLease(), opts...)
	if refusal != "" {
		e.Refuse(refusal)
		return e
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := e.Tick(tctx); err != nil {
		logWarn("leader", "first lease attempt failed, retrying: %v", err)
	}
	cancel()
	go e.Run(ctx)
	return e
}
