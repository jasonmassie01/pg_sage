package main

import (
	"context"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/rollout"
)

const fleetRolloutPollInterval = 6 * time.Hour

var fleetRolloutFactoryState struct {
	sync.RWMutex
	factory rollout.RuntimeFactory
}

func setFleetRolloutRuntimeFactory(factory rollout.RuntimeFactory) {
	fleetRolloutFactoryState.Lock()
	defer fleetRolloutFactoryState.Unlock()
	fleetRolloutFactoryState.factory = factory
}

func currentFleetRolloutRuntime(ctx context.Context) (*rollout.Runtime, error) {
	fleetRolloutFactoryState.RLock()
	factory := fleetRolloutFactoryState.factory
	fleetRolloutFactoryState.RUnlock()
	if factory == nil {
		return nil, rollout.ErrRuntimeDeferred
	}
	return factory(ctx)
}

func startFleetRolloutScheduler() {
	if cfg == nil || (!cfg.IsFleet() && !cfg.HasMetaDB()) {
		return
	}
	go rollout.RunPeriodic(
		shutdownCtx, fleetRolloutPollInterval, currentFleetRolloutRuntime,
		func(err error) {
			logWarn("rollout", "production fleet rollout unavailable: %v", err)
		},
	)
}
