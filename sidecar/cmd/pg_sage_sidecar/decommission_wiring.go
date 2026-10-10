package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/decommission"
)

// decommissionStartupTimeout bounds the startup inventory; it reads a few
// small tables and two catalogs.
const decommissionStartupTimeout = 30 * time.Second

// startDecommissionReport logs the removed provisioner's inventory on the
// control database in the background (AGENTDB-SPEC §12 steps 2-4). A failed
// inventory is an error in the log, never a refused start: the inventory is
// a report, and the API serves it again on demand.
func startDecommissionReport(ctx context.Context, pool *pgxpool.Pool, configPath string) {
	go runDecommissionReport(ctx, pool, configPath)
}

func runDecommissionReport(ctx context.Context, pool *pgxpool.Pool, configPath string) {
	rctx, cancel := context.WithTimeout(ctx, decommissionStartupTimeout)
	defer cancel()
	logger := decommission.Logger{
		Info: func(format string, args ...any) { logInfo("decommission", format, args...) },
		Warn: func(format string, args ...any) { logWarn("decommission", format, args...) },
	}
	if err := decommission.Startup(rctx, pool, configPath, logger); err != nil {
		logError("decommission", "startup inventory failed (%s; retry with GET %s): %v",
			decommission.SpecRef, decommission.InventoryPath, err)
	}
}
