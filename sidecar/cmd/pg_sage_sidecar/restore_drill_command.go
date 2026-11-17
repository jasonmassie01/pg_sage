package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Self-managed restore drills (drill-01c): with drill.command.restore_argv
// set, every drill runs the operator's restore program instead of
// clone.provider, and the clone reaper destroys the clones a crashed
// drill left with destroy_argv. drill.command is restart-bound, so the
// config the process started with is the one both use.

// commandDrillWorkDir is where the restore and destroy programs run.
func commandDrillWorkDir(c *config.Config) string {
	return filepath.Join(c.DataDir, "restore-drills")
}

// commandDrillAdapter is the restore command provider of a drill of
// database on source, bounded by the drill's max_duration_minutes; a nil
// source is the reaper's destroy-only use.
func commandDrillAdapter(c *config.Config, database string, source *pgxpool.Pool,
	maxMinutes int64) (clone.Provider, error) {
	cmd := c.Drill.Command
	cfg := clone.CommandConfig{RestoreArgv: cmd.RestoreArgv, DestroyArgv: cmd.DestroyArgv,
		EnvAllow: cmd.EnvAllow, WorkDir: commandDrillWorkDir(c),
		MaxDuration: time.Duration(maxMinutes) * time.Minute}
	if source != nil {
		cfg.SourceDSN = source.Config().ConnString()
	}
	cfg.Database = database
	return clone.NewCommandProvider(cfg)
}

// drillAdapter is inst's drill adapter with its receipt adapter name and
// scope: the restore command when one is configured, else clone.provider.
func drillAdapter(c *config.Config, cloneCfg config.CloneProviderConfig,
	inst drillInstance, maxMinutes int64) (clone.Provider, string, string, error) {
	if c.Drill.Command.Configured() {
		adapter, err := commandDrillAdapter(c, inst.name, inst.pool, maxMinutes)
		return adapter, clone.CommandAdapter, clone.CommandScope(c.Drill.Command.RestoreArgv),
			err
	}
	adapter, err := cloneProviderFor(cloneCfg, inst.pool)
	return adapter, cloneCfg.Provider, cloneReceiptScope(cloneCfg), err
}

// restoreCommandStartupLine names the programs and the variable names
// they may read, never an argument or a value; "" without a command.
func restoreCommandStartupLine(cmd config.DrillCommandConfig) string {
	if !cmd.Configured() {
		return ""
	}
	program := func(argv []string) string {
		if len(argv) == 0 {
			return "(none)"
		}
		return argv[0]
	}
	names := "none"
	if len(cmd.EnvAllow) > 0 {
		names = strings.Join(cmd.EnvAllow, ", ")
	}
	return fmt.Sprintf("restore drills run the restore command %s (destroy: %s; "+
		"environment names passed: %s)", program(cmd.RestoreArgv), program(cmd.DestroyArgv),
		names)
}

// startCommandDrillReaper reaps drill receipts of the restore command
// every cloneReapInterval on the leader, when one is configured.
func startCommandDrillReaper(ctx context.Context, c *config.Config,
	mgr *fleet.DatabaseManager, meta *metaDBState) {
	if c == nil || !c.Drill.Command.Configured() {
		return
	}
	go every(ctx, cloneReapInterval, func(ctx context.Context) {
		if !fleetLeaderAllows("restore command clone reaper") {
			return
		}
		for _, pool := range cloneReceiptPools(c, mgr, meta) {
			reapCommandDrillClones(ctx, c, pool)
		}
	})
	logInfo("clone", "restore command clone reaper started (every %s)", cloneReapInterval)
}

// reapCommandDrillClones runs one reaper pass over the restore command's
// drill receipts in pool.
func reapCommandDrillClones(ctx context.Context, c *config.Config,
	pool *pgxpool.Pool) clone.ReapReport {
	deployment, err := earned.EnsureDeployment(ctx, pool)
	if err != nil {
		logWarn("clone", "restore command clone reaper: deployment id: %v", err)
		return clone.ReapReport{}
	}
	// The destroy program only: no restore runs here, so any bound works.
	adapter, err := commandDrillAdapter(c, "", nil, 1)
	if err != nil {
		logError("clone", "restore command clone reaper is off: %v", err)
		return clone.ReapReport{}
	}
	report, err := clone.Reaper{Store: clone.NewReceiptStore(pool), Provider: adapter,
		Scope: clone.ReceiptScope{DeploymentID: deployment, Adapter: clone.CommandAdapter,
			Scope: clone.CommandScope(c.Drill.Command.RestoreArgv)},
		Purpose: clone.PurposeDrill, Holder: clone.ProcessHolder()}.Reap(ctx)
	if err != nil {
		logWarn("clone", "restore command clone reaper: %v", err)
	}
	logReapReport(deployment, report)
	return report
}
