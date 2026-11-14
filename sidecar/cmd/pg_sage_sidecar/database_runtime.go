package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/briefing"
	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/ha"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/providerobs"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/selfbudget"
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
// it into the same runtime in standalone, YAML fleet and meta-db mode
// (G5-I07).
type databaseRuntimeSpec struct {
	// Scope names the mode in logs: startup, fleet or meta-db.
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
	// ActionPool is the separate action identity (action_dsn): changes and
	// backend signals run on it. Nil runs everything on Pool. The caller
	// owns it like Pool; the fleet registration closes it with Pool.
	ActionPool *pgxpool.Pool
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
	probed     string // the probe outcome before any provider override
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
	// lockObs is the latest lock-chain observation, shared by the RCA fast
	// path, the analyzer cycle and the migration detector (migration-05);
	// nil without RCA.
	lockObs *analyzer.LockChainObserver
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
	// tempDetector is the investigator's reactive detector (nil without
	// one); self-configuration samples its temp-file window peaks.
	tempDetector *sre.ReactiveDetector
	// sloEngine and changeFeed are the M5 signals (nil when off).
	sloEngine  *slo.Engine
	changeFeed *changefeed.Feed
	// runwayAdvisor attaches custodian proposals to runway investigations.
	runwayAdvisor *runwayAdvisor
	// ha is the database's one HA monitor (ha_loop.go).
	ha        *ha.Monitor
	logFanout *logwatch.LogFanout
	// budgetMonitor is the workload budget detector, fed the log's
	// statement-timeout cancels once the fanout exists (market-08b).
	budgetMonitor *budget.Monitor
	// providerLogs is set when RCA reads the provider's log API instead of
	// the cluster's log files (Supabase).
	providerLogs atomic.Bool
	brief        *briefing.Worker
	features     []string
	inst         *fleet.DatabaseInstance
	// firstLook is the database's first look, kept for operator re-checks
	// (firstlook-05).
	firstLook *firstLookRun
	// facts is the database's fact store (roadmap 2.3); factCluster is its
	// place among the process's databases of the same cluster (facts-02).
	facts       *facts.Store
	factCluster *factClusterMember
	// instanceOwner is the runtime's place among the process's databases
	// of one instance (advisors-08); nil in single-database mode.
	instanceOwner *instanceMember
	// cloud is managed-cloud host telemetry and cloudResolver the managed
	// change target (nil when the provider has none).
	lakebaseScope atomic.Value // verified immutable resource/policy pair, read by history
	cloud         *cloudtel.Runtime
	cloudResolver managedparam.Resolver
	// backupNote logs a withheld backup freshness once per reason.
	backupNote reasonLatch
	// azureParams is the database's Azure ARM adapter (nil off Azure or
	// without the azure: identity): the managed-change worker reads server
	// parameters through it.
	azureParams executor.ManagedConfigAdapter

	// freeze is the database's one freeze custodian: its sampler is the
	// only horizon reader and rate state of the analyzer, the runway
	// advisor and the autonomy worker (vacuum-08).
	freeze     *autonomy.PostgresFreezeCustodian
	freezeOnce sync.Once
	// providerObs is Supabase observability (nil: not running); its memory
	// backs the gates when there is no managed-cloud telemetry.
	providerObs atomic.Pointer[providerobs.Runtime]
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
	rt.startHA()
	if err := rt.installHistory(ctx); err != nil { // history_store_wiring.go
		rt.cancel()
		return nil, err
	}
	rt.initOnboarding(ctx)
	rt.startFirstLook()
	rt.startMonitoring()
	rt.startExecution()
	rt.startFacts()
	rt.startSREActions()
	rt.startActionOutcomeFeed() // M7: approved M5 runs feed the ledger
	rt.startRunways()
	rt.startAsk()
	rt.startSourceFixVerifier() // mcp-06: verdicts decided in the background
	rt.startSelfConfig()
	rt.logExecutorSettings()
	rt.inst = rt.instance()
	rt.startMetricsRefresher() // metrics_refresher.go
	rt.startCapabilityRefresh()
	rt.startProviderReprobe()
	rt.startDeferredIndexBuilder() // config-04c: large sage indexes, in the background
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
// config migration), warns on a non-read-committed isolation default
// (audit-02) and checks prerequisites. A failed bootstrap always
// refuses the database, because nothing can persist without the schema
// (G5-B16); a failed check either refuses it or degrades it (G5-B13).
func prepareMonitoredDatabase(
	ctx context.Context, pool *pgxpool.Pool, name string, requireChecks bool,
) (*startup.CheckResult, error) {
	if err := bootstrapManagedDatabaseSchema(ctx, pool); err != nil {
		return nil, fmt.Errorf("bootstrap schema for %q: %w", name, err)
	}
	if pool != nil {
		warnIsolationDefault(ctx, pool, name, logWarn)
	}
	checks, err := instanceChecksOrDegraded(ctx, pool)
	// A missing pg_stat_statements degrades even a required database: the
	// first look and the catalog rules need no query statistics, and they
	// tell the operator exactly how to enable it (five-minute time to value).
	if err != nil && requireChecks && !errors.Is(err, startup.ErrStatementsUnavailable) {
		return nil, fmt.Errorf("prerequisite checks for %q: %w", name, err)
	}
	reportChecks(name, checks, err, logWarn)
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
	// Every worker's loop is tracked under this database (footprint-07).
	parent = selfbudget.WithDatabase(parent, spec.Name)
	if spec.ControlPool == nil {
		spec.ControlPool = spec.Pool
	}
	rt := &databaseRuntime{spec: spec, checks: checks, workers: &sync.WaitGroup{}}
	rt.ctx, rt.cancel = context.WithCancel(parent)
	rt.initFacts()
	rt.provider = rt.detectProvider(parent)
	rt.cfg = rt.runtimeConfig()
	rt.probes = probes.NewRunner(spec.Pool, probes.Catalog(), sreProbeLimiter, rt.sreReadPolicy())
	rt.probes.SetDatabaseName(spec.Name)
	rt.deriveSettingsAtStartup()
	rt.initCloudTelemetry()
	rt.resolveLLM()
	rt.dispatcher = sharedNotifyDispatcher(spec.ControlPool)
	rt.registerFactCards()
	rt.ha = newInstanceHAMonitor(spec.Pool, spec.ControlPool, spec.Name,
		rt.recordID(), haCadence(rt.cfg), rt.cfg.SRE.Autonomy.FailoverCooldown())
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
	withCapabilityFlags(cfg, rt.checks)
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
	if rt.llmManager != nil {
		rt.attachPolicyCeiling(rt.llmManager.General, rt.llmManager.Optimizer)
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

// recordID is the database's control-plane id, nil without one.
func (rt *databaseRuntime) recordID() *int {
	if rt.spec.DatabaseID <= 0 {
		return nil
	}
	id := rt.spec.DatabaseID
	return &id
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
		Config: rt.spec.Config, Pool: rt.spec.Pool, ActionPool: rt.spec.ActionPool,
		Collector: rt.collector, Analyzer: rt.analyzer, Executor: rt.executor,
		HA:             rt.ha,
		Investigations: rt.sreService,
		SLO:            rt.sloEngine,
		Changes:        rt.changeFeed,
		Actions:        rt.sreActions,
		LogSource:      rt.logSourceStatus,
		Cancel:         rt.cancel, Workers: rt.workers,
		ExecutorShutdown: rt.executor.Shutdown,
		// Removal and replacement let in-flight actions finish first.
		Quiesce:          rt.executor.Quiesce,
		InstanceOwner:    rt.instanceOwnership(),
		FirstLookRecheck: rt.firstLookRecheck(),
		DiskCapacity:     rt.forecastCapacity(),
		Status: &fleet.InstanceStatus{
			Connected:    true,
			Platform:     rt.provider,
			TrustLevel:   rt.executor.TrustLevel(),
			DatabaseName: rt.spec.Name,
			LastSeen:     time.Now(),
			// Unknown until the first status refresh reads them (cases-11).
			IncidentsCritical: fleet.IncidentsUnknown,
			IncidentsWarning:  fleet.IncidentsUnknown,
			Capabilities: fleet.CollectProviderCapabilities(
				rt.ctx, rt.spec.Pool, rt.provider,
				fleet.ExecutorFamilyExplainer(rt.executor),
			),
		},
	}
}
