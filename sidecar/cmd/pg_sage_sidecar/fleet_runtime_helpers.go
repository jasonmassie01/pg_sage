package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/startup"
)

// initializeAnalyzeSemaphore sizes the process-wide ANALYZE semaphore from
// the tuner config at mode startup.
func initializeAnalyzeSemaphore() {
	analyzeSemMu.Lock()
	defer analyzeSemMu.Unlock()
	analyzeSem = newAnalyzeSemaphore()
}

func newAnalyzeSemaphore() chan struct{} {
	maxAnalyze := cfg.Tuner.MaxConcurrentAnalyze
	if maxAnalyze <= 0 {
		maxAnalyze = 1
	}
	logInfo("startup", "ANALYZE semaphore sized to %d concurrent slots",
		maxAnalyze)
	return make(chan struct{}, maxAnalyze)
}

// newFleetDBLLMClients builds the registry-tracked general and optimizer
// clients for one database. allowed=false (databases[].llm_enabled: false)
// yields clients that stay disabled across every reconfigure (G5-B07).
func newFleetDBLLMClients(
	databaseName string, allowed bool,
) (*llm.Client, *llm.Manager) {
	general := llmClients.newClient(llmRoleGeneral, databaseName, allowed)
	attachFleetBudget(general, databaseName)
	optimizerClient := general
	if cfg.LLM.OptimizerLLM.Enabled {
		optimizerClient = llmClients.newClient(
			llmRoleOptimizer, databaseName, allowed,
		)
		attachFleetBudget(optimizerClient, databaseName)
	}
	return general, llm.NewManager(
		general, optimizerClient, cfg.LLM.OptimizerLLM.FallbackToGeneral,
	)
}

// attachFleetBudget registers the database with the fleet budget (so
// databases added at runtime get a share, G5-B06) and scopes the client.
func attachFleetBudget(client *llm.Client, databaseName string) {
	if fleetLLMBudget == nil {
		return
	}
	fleetLLMBudget.Register(databaseName)
	client.SetBudget(dbBudget{b: fleetLLMBudget, db: databaseName})
}

func initializeFleetBudget(databaseNames []string) {
	fleetLLMBudget = nil
	if cfg.LLM.FleetTokenBudgetDaily <= 0 {
		return
	}
	fleetLLMBudget = fleet.NewBudget(
		cfg.LLM.FleetTokenBudgetDaily, databaseNames,
	)
	go resetFleetBudgetDaily(shutdownCtx, fleetLLMBudget)
	logInfo("fleet", "per-database LLM budget enabled: "+
		"%d tokens/day across %d databases",
		cfg.LLM.FleetTokenBudgetDaily, len(databaseNames))
}

// runInstanceChecks validates one monitored database's prerequisites. A
// variable so tests can stub it.
var runInstanceChecks = startup.RunChecks

// detectInstanceVersion probes server_version_num; a variable for tests.
var detectInstanceVersion = detectPGVersion

// instanceChecksOrDegraded runs the per-database prerequisite checks. A
// failure (for example pg_stat_statements not installed) keeps the
// database monitored in degraded mode — capability flags off — and is
// returned as a warning. Dropping the database instead also skipped the
// first-admin bootstrap and locked the dashboard.
func instanceChecksOrDegraded(
	ctx context.Context, pool *pgxpool.Pool,
) (*startup.CheckResult, error) {
	checks, err := runInstanceChecks(ctx, pool)
	if err == nil && checks != nil {
		return checks, nil
	}
	if err == nil {
		err = errors.New("prerequisite checks returned no result")
	}
	return &startup.CheckResult{PGVersionNum: detectInstanceVersion(pool)}, err
}

// instanceRuntimeConfig clones the global config and carries the database's
// own capability flags, so fleet and meta collectors select WAL and
// plan-time columns per instance (G5-B13, G1-B07).
func instanceRuntimeConfig(checks *startup.CheckResult) *config.Config {
	runtimeCfg := config.Clone(cfg)
	runtimeCfg.PGVersionNum = checks.PGVersionNum
	runtimeCfg.HasWALColumns = checks.HasWALColumns
	runtimeCfg.HasPlanTimeColumns = checks.HasPlanTimeColumns
	return runtimeCfg
}
