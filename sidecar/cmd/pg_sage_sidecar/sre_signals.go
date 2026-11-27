package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// sreSignals is one database's Sage SRE M5 signals: its change feed (and
// the poller of pg_sage's own change sources) and its SLO engine. They
// bind to the investigator's database scope, feed every investigation
// (change_feed and slo_status evidence) and open an slo_burn
// investigation on a page-level burn.
type sreSignals struct {
	name     string
	settings config.SREConfig
	feed     *changefeed.Feed
	poller   *changefeed.Poller
	// providerPoller polls the provider-event sources alone (restart,
	// failover, version, host, provider API) when feed polling is off.
	providerPoller  *changefeed.Poller
	engine          *slo.Engine
	applicationOnly bool
	// owners maps an SLO to its owner_database: only that database's
	// investigator opens the SLO's slo_burn investigations.
	owners map[string]string
	coord  atomic.Pointer[sre.Coordinator]
	logFn  func(string, string, ...any)
}

type sreSignalsDeps struct {
	readPolicy *probes.ReadPolicy
	control    *pgxpool.Pool
	monitored  *pgxpool.Pool
	name       string
	legacyID   *int
	settings   config.SREConfig
	// errors counts server-class log errors; nil without a log source.
	errors  slo.ErrorCounter
	history probes.HistoricalSource
	logFn   func(string, string, ...any)
	// managed: the database is a managed service (provider attribution);
	// providerAPI reads its provider's event API (nil: none).
	managed     bool
	providerAPI changefeed.ProviderEventReader
}

// newSRESignals builds the feed (always: ingested events are read even
// when polling is off), the poller (sre.change_events.feed_enabled; the
// provider-event sources alone when it is off) and the SLO engine
// (sre.slo.enabled).
func newSRESignals(d sreSignalsDeps) (*sreSignals, error) {
	s := &sreSignals{name: d.name, settings: d.settings, logFn: d.logFn}
	cs, err := changefeed.NewStore(d.control)
	if err != nil {
		return nil, err
	}
	s.feed = changefeed.NewFeed(cs, d.name, s.scope)
	if err := s.newPoller(d); err != nil {
		return nil, err
	}
	s.applicationOnly = !d.readPolicy.Allows(probes.SLOStatus) && d.readPolicy.Allows(probes.ApplicationSLOStatus)
	if d.settings.SLO.Enabled && (d.readPolicy.Allows(probes.SLOStatus) || s.applicationOnly) {
		if s.engine, err = s.newEngine(d); err != nil {
			return nil, err
		}
	}
	if d.settings.SLO.Enabled && s.logFn != nil {
		for _, name := range unevaluatedOwnedSLOs(d.settings.SLO, d.name, s.owners) {
			s.logFn("WARN", "sre slo: %s names %s as owner_database, but the read "+
				"policy of %s does not evaluate it; its page-level burns open no "+
				"slo_burn investigation anywhere (bind it with database, or name an "+
				"owner whose read policy allows slo_status)", name, d.name, d.name)
		}
	}
	return s, nil
}

// newPoller builds the change feed poller the read policy allows: every
// source with feed polling on, the provider-event sources alone with it
// off (guard-02b: provider changes are always on the timeline).
func (s *sreSignals) newPoller(d sreSignalsDeps) error {
	if !d.readPolicy.Allows(probes.WALRunwayProbe) {
		return nil
	}
	ce := d.settings.ChangeEvents
	p, err := changefeed.NewPoller(changefeed.PollerDeps{Feed: s.feed,
		Monitored: d.monitored, Control: d.control, LegacyDatabaseID: d.legacyID,
		Interval: ce.FeedInterval(), Retention: ce.Retention(), Logf: d.logFn,
		Managed: d.managed, ProviderAPI: d.providerAPI,
		ProviderEventsOnly: !ce.FeedEnabled})
	if err != nil {
		return err
	}
	if ce.FeedEnabled {
		s.poller = p
	} else {
		s.providerPoller = p
	}
	return nil
}

func (s *sreSignals) newEngine(d sreSignalsDeps) (*slo.Engine, error) {
	cfg := d.settings.SLO
	if s.applicationOnly {
		// A database read policy cannot authorize deployment-wide Prometheus
		// queries or cluster proxies. Only explicitly bound push counters qualify.
		cfg.Objectives = databasePushObjectives(cfg.Objectives, d.name)
		cfg.Prometheus = config.SREPrometheusConfig{}
		cfg.Proxies.Enabled = false
	}
	objs, err := sloObjectives(cfg, d.name)
	if err != nil {
		return nil, err
	}
	s.owners = sloOwners(objs)
	rules, err := sloRules(cfg)
	if err != nil {
		return nil, err
	}
	prom, err := prometheusClient(cfg.Prometheus)
	if err != nil {
		return nil, err
	}
	ss, err := slo.NewStore(d.control)
	if err != nil {
		return nil, err
	}
	var proxies []slo.Proxy
	if cfg.Proxies.Enabled {
		pc := sloProxyConfig(cfg.Proxies)
		errorProxy := slo.NewErrorProxy(d.monitored, d.errors, pc)
		if d.history != nil && d.errors == nil {
			errorProxy = slo.NewHistoricalErrorProxy(d.history, pc)
		}
		proxies = []slo.Proxy{slo.NewLatencyProxy(d.monitored, pc),
			errorProxy, slo.NewConnectionProxy(d.monitored, pc),
			slo.NewReplicationLagProxy(d.monitored, pc)}
	}
	return slo.NewEngine(slo.EngineDeps{Database: d.name, Objectives: objs, Proxies: proxies,
		DatabaseScopedPush: s.applicationOnly || config.DatabricksAppMode(),
		Rules:              rules, Store: ss, Prometheus: prom, Scope: s.scope, OnPage: s.onPage,
		Interval: cfg.EvaluationInterval(), Logf: d.logFn})
}

// probes are the signal probes every investigation collects.
func (s *sreSignals) probes() []sre.SignalProbe {
	out := []sre.SignalProbe{{ID: probes.ChangeFeed, Run: s.feed.Probe}}
	if s.engine != nil {
		id := probes.SLOStatus
		if s.applicationOnly {
			id = probes.ApplicationSLOStatus
		}
		out = append(out, sre.SignalProbe{ID: id, Run: func(ctx context.Context, args probes.Args) probes.Result {
			r := s.engine.Probe(ctx, args)
			r.ProbeID = id
			return r
		}})
	}
	return out
}

func databasePushObjectives(objectives []config.SREObjectiveConfig, database string) []config.SREObjectiveConfig {
	var out []config.SREObjectiveConfig
	for _, o := range objectives {
		if o.Database == database && o.Source == "push" {
			out = append(out, o)
		}
	}
	return out
}

// attach binds the signals to the database's investigator.
func (s *sreSignals) attach(c *sre.Coordinator) { s.coord.Store(c) }

// scope is the investigator's database scope.
func (s *sreSignals) scope(ctx context.Context) (sre.Scope, error) {
	c := s.coord.Load()
	if c == nil {
		return sre.Scope{}, fmt.Errorf("%w: investigator of %s not built",
			sre.ErrMetadataUnavailable, s.name)
	}
	return c.Bind(ctx)
}

// onPage opens (or coalesces into) the slo_burn investigation of a
// page-level burn: always for a registered app SLI, for a database proxy
// only with sre.automatic_start (R1 keeps automatic starts opt-in), and
// never with sre.slo.open_investigations off. A fleet SLO with an
// owner_database pages only on its owner (each database evaluates it).
func (s *sreSignals) onPage(ctx context.Context, st slo.Status) error {
	if !s.settings.SLO.OpenInvestigations || st.BurnStartedAt == nil ||
		(st.Kind == slo.KindProxy && !s.settings.AutomaticStart) {
		return nil
	}
	if owner := s.owners[st.Name]; owner != "" && owner != s.name {
		s.logFn("INFO", "sre slo: %s burns its error budget at page level on %s; "+
			"burn owned by %s, whose investigator opens the slo_burn investigation",
			st.Name, s.name, owner)
		return nil
	}
	c := s.coord.Load()
	if c == nil {
		return fmt.Errorf("%w: investigator of %s not built", sre.ErrMetadataUnavailable,
			s.name)
	}
	if _, err := c.Bind(ctx); err != nil {
		return err
	}
	_, created, err := c.Start(ctx, sre.Trigger{CaseID: "slo:" + st.Name,
		Kind: sre.TriggerSLOBurn, Subject: "slo " + st.Name,
		IdempotencyKey: fmt.Sprintf("slo:%s:%d", st.Name, st.BurnStartedAt.Unix())})
	if err == nil && created {
		s.logFn("WARN", "sre slo: %s of %s burns its error budget at page level; "+
			"opened a read-only slo_burn investigation", st.Name, s.name)
	}
	return err
}

// start runs the poller and the engine on the runtime's worker group.
func (s *sreSignals) start(run func(func()), ctx context.Context) {
	for _, p := range []*changefeed.Poller{s.poller, s.providerPoller} {
		if p != nil {
			run(func() { p.Run(ctx) })
		}
	}
	if s.engine != nil {
		run(func() { s.engine.Run(ctx) })
	}
}

// logErrorCounter counts server-class errors in the database's log
// entries. While its source is not reading the log (unavailable, retrying)
// the count is unknown, never a healthy zero.
type logErrorCounter struct {
	sub    *logwatch.EntrySubscriber
	source interface{ Active() bool }
}

func (c logErrorCounter) DrainErrors() (int64, bool) {
	if c.source == nil || !c.source.Active() {
		c.sub.Drain()
		return 0, false
	}
	var n int64
	for _, e := range c.sub.Drain() {
		if slo.ServerErrorClass(e.SQLState) {
			n++
		}
	}
	return n, true
}
