package main

import (
	"sync/atomic"

	"github.com/pg-sage/sidecar/internal/leader"
)

// fleetLeaderRef holds the fleet's leader elector, nil when election is
// disabled (every sidecar runs fleet-wide jobs). startFleetLearning sets it
// once the control pool is up, after the database runtimes started, and
// their loops (pressure enforcement, executor, plan engine) read it on every
// tick: an atomic pointer, so that read never races with the set.
var fleetLeaderRef atomic.Pointer[leader.Elector]

// currentFleetLeader is the fleet's elector, or nil when election is off.
func currentFleetLeader() *leader.Elector { return fleetLeaderRef.Load() }

// setFleetLeader installs the fleet's elector; nil turns election off.
func setFleetLeader(e *leader.Elector) { fleetLeaderRef.Store(e) }
