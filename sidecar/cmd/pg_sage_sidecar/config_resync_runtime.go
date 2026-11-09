package main

import (
	"context"
	"errors"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// configResyncInterval bounds how long a replica serves a global setting
// another replica replaced; each tick is one generation query (config-05).
const configResyncInterval = 30 * time.Second

// configResyncTimeout bounds one tick, including the owners' reconfiguration.
const configResyncTimeout = 10 * time.Second

// resyncConfigFromStore adopts the global generation another replica saved
// to the shared control database. It reports whether this replica moved; a
// failure is logged and keeps the current config.
func resyncConfigFromStore(ctx context.Context) bool {
	controller := configController
	if controller == nil {
		return false
	}
	result, adopted, err := controller.ResyncFromDurable(ctx)
	if errors.Is(err, config.ErrInvalidCandidate) {
		logError("config", "the durable global config is invalid on this replica; "+
			"keeping generation %d: %v (fix the key in Settings, or "+
			"DELETE /api/v1/config/global/<key> removes a stored override)",
			controller.Desired().Generation, err)
		return false
	}
	if err != nil {
		logWarn("config", "resync with the durable config failed; keeping generation %d: %v",
			controller.Desired().Generation, err)
		return false
	}
	if !adopted {
		return false
	}
	logInfo("config", "resynced to the durable generation desired=%d active=%d "+
		"pending_restart=%d", result.DesiredGeneration, result.ActiveGeneration,
		len(result.PendingRestart))
	for _, warning := range result.Warnings {
		logWarn("config", "resync: %s", warning)
	}
	return true
}

// configResyncLoop polls the durable generation every interval until ctx
// ends.
func configResyncLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			tickCtx, cancel := context.WithTimeout(ctx, configResyncTimeout)
			resyncConfigFromStore(tickCtx)
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

// startConfigResync starts the poll where a control database shares the
// global overrides: meta mode, and standalone replicas on one database. A
// YAML fleet has none and polls nothing. It reports whether it started.
func startConfigResync(ctx context.Context) bool {
	if configController == nil || configControlPool() == nil {
		return false
	}
	go configResyncLoop(ctx, configResyncInterval)
	return true
}
