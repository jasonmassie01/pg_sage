package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/sre"
)

// sreBindTimeout bounds binding the database identity at startup; the
// coordinator loop retries a failed binding.
const sreBindTimeout = 5 * time.Second

// sreInvestigatorDeps is one database's Sage SRE investigator wiring.
type sreInvestigatorDeps struct {
	// control holds the sage.sre_* coordination tables: the meta database
	// when one is configured, otherwise the monitored database.
	control *pgxpool.Pool
	// monitored holds the incidents and findings that trigger work.
	monitored  *pgxpool.Pool
	runner     sre.ProbeRunner
	name       string
	runtimeKey string
	legacyID   *int
	settings   config.SREConfig
	logFn      func(string, string, ...any)
	// llm is the database's general LLM client (nil without one) and
	// dailyTokens its llm.token_budget_daily, the model turn's daily
	// allocation. notices is nil for the process-wide once-log.
	llm         *llm.Client
	dailyTokens int
	notices     *sre.OnceLog
	// signals (M5) are the change feed and SLO status probes; nil keeps
	// the M2 plans.
	signals []sre.SignalProbe
	// advisor attaches custodian proposals to runway investigations.
	advisor sre.ActionAdvisor
	// episodes records reactive detector episodes as incidents (the
	// database's RCA engine); nil without RCA.
	episodes sre.EpisodeSink
	// rootAuthority decides per family whether the model may override a
	// conclusive graph root (roadmap 2.4); nil keeps model roots advisory.
	rootAuthority sre.RootAuthority
}

// newSREInvestigator builds one database's investigator (coordinator,
// trigger adapter and read service) and binds its database identity.
func newSREInvestigator(d sreInvestigatorDeps) (*sre.Service, error) {
	notices := d.notices
	if notices == nil {
		notices = sre.ModelNotices
	}
	model := sreModelClient(d.settings, d.llm, notices, d.logFn)
	store, err := sre.NewPostgresStore(d.control,
		sreLimits(model, d.dailyTokens, notices, d.logFn))
	if err != nil {
		return nil, fmt.Errorf("sre store: %w", err)
	}
	cc := sre.DefaultCoordinatorConfig(d.runtimeKey)
	cc.LegacyDatabaseID = d.legacyID
	cc.AutomaticStart = d.settings.AutomaticStart
	cc.TriggerInterval = d.settings.TriggerInterval()
	cc.SampleInterval = d.settings.SampleInterval()
	cc.Retention.EvidenceAge = d.settings.EvidenceRetention()
	cc.Retention.TimelineAge = d.settings.TimelineRetention()
	cc.RunwayWindow = d.settings.Runways.Lookback()
	triggers, err := sreTriggerSource(d)
	if err != nil {
		return nil, err
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: store, Runner: d.runner,
		Triggers: triggers, Config: cc, LogFn: d.logFn, Model: model, Notices: notices,
		Signals: d.signals, Advisor: d.advisor, RootAuthority: d.rootAuthority})
	if err != nil {
		return nil, fmt.Errorf("sre coordinator: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sreBindTimeout)
	defer cancel()
	if _, err := coord.Bind(ctx); err != nil {
		d.logFn("WARN", "sre: db %q: binding the database identity failed "+
			"(retried by the investigator loop): %v", d.name, err)
	}
	return sre.NewService(d.name, coord, store), nil
}

// startInvestigator runs the database's Sage SRE investigator on the
// instance worker group, in every mode (CHECK-31).
func (rt *databaseRuntime) startInvestigator() {
	key, legacy := rt.sreRuntimeKey()
	signals := rt.newSignals(legacy)
	var signalProbes []sre.SignalProbe
	if signals != nil {
		signalProbes = signals.probes()
	}
	signalProbes = append(signalProbes, poolerSignalsFor(rt.cfg.SRE.Poolers, rt.spec.Name,
		logStructuredWrapper)...)
	rt.runwayAdvisor = newRunwayAdvisorFor(rt.spec.Pool, rt.cfg, rt.spec.Name)
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: rt.spec.ControlPool,
		monitored: rt.spec.Pool, runner: rt.probes, name: rt.spec.Name,
		runtimeKey: key, legacyID: legacy, settings: rt.cfg.SRE,
		logFn: logStructuredWrapper, llm: rt.generalLLM,
		dailyTokens: rt.cfg.LLM.TokenBudgetDaily, signals: signalProbes,
		advisor: rt.runwayAdvisor, episodes: rt.detectorIncidents(),
		rootAuthority: registryRootAuthority{registry: processAutonomy().registry,
			database: rt.spec.Name}})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: sre investigator not started: %v", rt.spec.Name, err)
		return
	}
	rt.sreService, rt.sre = svc, svc.Coordinator()
	rt.start(func() { rt.sre.Run(rt.ctx) })
	rt.sreStarted = true
	rt.note("sre_investigator")
	rt.startSignals(signals)
}

// sreRuntimeKey is the stable key bound to the database UUID: the meta-db
// record id when there is one, otherwise the mode and instance name.
func (rt *databaseRuntime) sreRuntimeKey() (string, *int) {
	if rt.spec.DatabaseID > 0 {
		id := rt.spec.DatabaseID
		return fmt.Sprintf("db:%d", id), &id
	}
	return rt.spec.Scope + ":" + rt.spec.Name, nil
}
