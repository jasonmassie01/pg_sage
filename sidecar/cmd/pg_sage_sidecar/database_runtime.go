package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/slo"
	"github.com/pg-sage/sidecar/internal/startup"
	"github.com/pg-sage/sidecar/internal/store"
)

// databaseRuntimeSpec is everything that legitimately differs between the
// deployment modes for one monitored database. buildDatabaseRuntime turns
// it into the same runtime in standalone, YAML fleet, meta-db and AgentDB
// mode (G5-I07).
type databaseRuntimeSpec struct {
	// Scope names the mode in logs: startup, fleet, meta-db or agentdb.
	Scope string
	// Name is the fleet instance name and every event's database_name.
	Name string
	// DatabaseID is the control-plane id. A positive id also scopes the
	// standing policy to this database (meta-db); zero uses the global one.
	DatabaseID int
	// Config carries the per-database overrides: trust level, executor and
	// LLM gates, and the PostgreSQL database name.
	Config config.DatabaseConfig
	Pool   *pgxpool.Pool
	// ControlPool holds the standing policy and the notification rules the
	// API writes (G5-B10, G5-B11). Nil means the monitored pool.
	ControlPool *pgxpool.Pool
	ExecMode    string
	// Parent bounds every worker; removing the database cancels only this
	// runtime.
	Parent context.Context
	// RequireChecks refuses a database whose prerequisite checks fail;
	// otherwise it is monitored degraded, with capability flags off.
	RequireChecks bool
	// Checks are prerequisite checks the caller already ran together with
	// the schema bootstrap. Nil runs both here.
	Checks *startup.CheckResult
	// Shared marks standalone mode, where the database is the process: its
	// runtime reads the live process config (hot reload) and LLM clients.
	Shared bool
}

// databaseRuntime is one database's components while they are assembled.
type databaseRuntime struct {
	spec       databaseRuntimeSpec
	ctx        context.Context
	cancel     context.CancelFunc
	workers    *sync.WaitGroup
	cfg        *config.Config
	checks     *startup.CheckResult
	provider   string
	generalLLM *llm.Client
	llmManager *llm.Manager
	llmOn      bool
	collector  *collector.Collector
	analyzer   *analyzer.Analyzer
	executor   *executor.Executor
	actions    *store.ActionStore
	dispatcher *notify.Dispatcher
	rca        *rca.Engine
	rcaAdapter *rcaAdapter
	// probes is the database's one catalog probe runner (at most one
	// probe at a time on it), shared by RCA narration and Sage SRE.
	probes *probes.Runner
	// sre is the Sage SRE investigator; sreStarted records that its loop
	// runs on the instance worker group.
	sre        *sre.Coordinator
	sreService *sre.Service
	// sreActions proposes, hands off and verifies Sage SRE actions (M5).
	sreActions *sreaction.ActionService
	sreStarted bool
	// sloEngine and changeFeed are the M5 signals (nil when off).
	sloEngine  *slo.Engine
	changeFeed *changefeed.Feed
	logFanout  *logwatch.LogFanout
	brief      *briefing.Worker
	features   []string
	inst       *fleet.DatabaseInstance
}

// buildDatabaseRuntime is the only per-database runtime constructor. It
// prepares the database, starts every monitoring and execution worker
// under one cancellable lifecycle and returns the fleet registration. The
// caller owns spec.Pool and closes it when an error is returned.
func buildDatabaseRuntime(
	ctx context.Context, spec databaseRuntimeSpec,
) (*databaseRuntime, error) {
	if spec.Pool == nil || spec.Name == "" {
		return nil, fmt.Errorf("database runtime needs a pool and a name")
	}
	checks, err := runtimePrerequisites(ctx, spec)
	if err != nil {
		return nil, err
	}
	migrateRecommendations(ctx, spec)
	rt := newDatabaseRuntime(spec, checks)
	rt.startMonitoring()
	rt.startExecution()
	rt.startSREActions()
	rt.logExecutorSettings()
	rt.inst = rt.instance()
	logInfo(spec.Scope, "db %q: initialized (%s)", spec.Name,
		strings.Join(rt.features, "+"))
	runtimeBuilt(rt)
	return rt, nil
}

// runtimeBuilt observes every completed runtime; parity tests replace it to
// inspect components the fleet registration does not expose.
var runtimeBuilt = func(*databaseRuntime) {}

// publish registers the runtime and fills its status at once, so the API
// never shows an empty status until the first orchestrator tick.
func (rt *databaseRuntime) publish(mgr *fleet.DatabaseManager) {
	mgr.RegisterInstance(rt.inst)
	updateInstanceFindings(rt.ctx, rt.inst)
}

// runtimePrerequisites runs the prerequisite checks and the schema
// bootstrap unless the caller already did.
func runtimePrerequisites(
	ctx context.Context, spec databaseRuntimeSpec,
) (*startup.CheckResult, error) {
	if spec.Checks != nil {
		return spec.Checks, nil
	}
	return prepareMonitoredDatabase(ctx, spec.Pool, spec.Name, spec.RequireChecks)
}

// prepareMonitoredDatabase bootstraps the sage schema (which includes the
// config migration) and checks prerequisites. A failed bootstrap always
// refuses the database, because nothing can persist without the schema
// (G5-B16); a failed check either refuses it or degrades it (G5-B13).
func prepareMonitoredDatabase(
	ctx context.Context, pool *pgxpool.Pool, name string, requireChecks bool,
) (*startup.CheckResult, error) {
	if err := bootstrapManagedDatabaseSchema(ctx, pool); err != nil {
		return nil, fmt.Errorf("bootstrap schema for %q: %w", name, err)
	}
	checks, err := instanceChecksOrDegraded(ctx, pool)
	if err != nil && requireChecks {
		return nil, fmt.Errorf("prerequisite checks for %q: %w", name, err)
	}
	if err != nil {
		logWarn("runtime", "db %q: prerequisite checks failed; monitoring "+
			"degraded: %v", name, err)
	} else if !checks.QueryTextVisible {
		logWarn("runtime", "db %q: query text not visible; GRANT "+
			"pg_read_all_stats to the monitoring role", name)
	}
	logInfo("runtime", "db %q: PG version %d, WAL columns: %v, plan_time "+
		"columns: %v", name, checks.PGVersionNum, checks.HasWALColumns,
		checks.HasPlanTimeColumns)
	return checks, nil
}
func newDatabaseRuntime(
	spec databaseRuntimeSpec, checks *startup.CheckResult,
) *databaseRuntime {
	parent := spec.Parent
	if parent == nil {
		parent = context.Background()
	}
	if spec.ControlPool == nil {
		spec.ControlPool = spec.Pool
	}
	rt := &databaseRuntime{spec: spec, checks: checks, workers: &sync.WaitGroup{}}
	rt.ctx, rt.cancel = context.WithCancel(parent)
	rt.probes = probes.NewRunner(spec.Pool, probes.Catalog(), sreProbeLimiter)
	rt.provider = detectCloudEnv(spec.Pool)
	logInfo(spec.Scope, "db %q: cloud environment: %s", spec.Name, rt.provider)
	rt.cfg = rt.runtimeConfig()
	rt.resolveLLM()
	rt.dispatcher = sharedNotifyDispatcher(spec.ControlPool)
	return rt
}

// runtimeConfig carries this database's capability flags, provider and
// load-admission attestation. Standalone writes them onto the live process
// config, which its runtime shares so hot reload keeps reaching it; other
// modes get a clone because each database has its own flags, provider and
// declared IO capacity (D6: databases[].verify.io_capacity).
func (rt *databaseRuntime) runtimeConfig() *config.Config {
	if !rt.spec.Shared {
		runtimeCfg := withCapabilityFlags(databaseExecConfig(cfg, rt.spec.Config), rt.checks)
		runtimeCfg.CloudEnvironment = rt.provider
		return runtimeCfg
	}
	cfg.PGVersionNum = rt.checks.PGVersionNum
	cfg.HasWALColumns = rt.checks.HasWALColumns
	cfg.HasPlanTimeColumns = rt.checks.HasPlanTimeColumns
	cfg.CloudEnvironment = rt.provider
	return cfg
}

// resolveLLM picks the process clients (standalone) or budget-scoped,
// registry-tracked clients that honour databases[].llm_enabled (G5-B07).
func (rt *databaseRuntime) resolveLLM() {
	if rt.spec.Shared {
		rt.generalLLM, rt.llmManager = llmClient, llmMgr
	} else {
		rt.generalLLM, rt.llmManager = newFleetDBLLMClients(
			rt.spec.Name, rt.spec.Config.IsLLMEnabled(),
		)
		releaseLLMClientsOnDone(rt.ctx, rt.llmManager)
	}
	rt.llmOn = rt.generalLLM != nil && rt.generalLLM.IsEnabled()
}

// start runs fn as an instance-owned worker that removal cancels and drains.
func (rt *databaseRuntime) start(fn func()) {
	startInstanceWorker(rt.workers, fn)
}

func (rt *databaseRuntime) note(feature string) {
	rt.features = append(rt.features, feature)
}

func (rt *databaseRuntime) pgVersion() int {
	return rt.checks.PGVersionNum
}

// instance is the fleet registration of this runtime generation. The
// server_version string is filled by the first status refresh, as for
// every mode (refreshCollectionStatus).
func (rt *databaseRuntime) instance() *fleet.DatabaseInstance {
	return &fleet.DatabaseInstance{
		Name: rt.spec.Name, DatabaseID: rt.spec.DatabaseID,
		Config: rt.spec.Config, Pool: rt.spec.Pool,
		Collector: rt.collector, Analyzer: rt.analyzer, Executor: rt.executor,
		Investigations: rt.sreService,
		SLO:            rt.sloEngine,
		Changes:        rt.changeFeed,
		Actions:        rt.sreActions,
		Cancel:         rt.cancel, Workers: rt.workers,
		ExecutorShutdown: rt.executor.Shutdown,
		Status: &fleet.InstanceStatus{
			Connected:    true,
			Platform:     rt.provider,
			TrustLevel:   rt.executor.TrustLevel(),
			DatabaseName: rt.spec.Name,
			LastSeen:     time.Now(),
			Capabilities: fleet.CollectProviderCapabilities(
				rt.ctx, rt.spec.Pool, rt.provider,
				fleet.ExecutorFamilyExplainer(rt.executor),
			),
		},
	}
}
