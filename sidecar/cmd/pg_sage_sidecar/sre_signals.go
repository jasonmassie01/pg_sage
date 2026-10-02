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
	engine   *slo.Engine
	coord    atomic.Pointer[sre.Coordinator]
	logFn    func(string, string, ...any)
}

type sreSignalsDeps struct {
	control   *pgxpool.Pool
	monitored *pgxpool.Pool
	name      string
	legacyID  *int
	settings  config.SREConfig
	// errors counts server-class log errors; nil without a log source.
	errors slo.ErrorCounter
	logFn  func(string, string, ...any)
}

// newSRESignals builds the feed (always: ingested events are read even
// when polling is off), the poller (sre.change_events.feed_enabled) and
// the SLO engine (sre.slo.enabled).
func newSRESignals(d sreSignalsDeps) (*sreSignals, error) {
	s := &sreSignals{name: d.name, settings: d.settings, logFn: d.logFn}
	cs, err := changefeed.NewStore(d.control)
	if err != nil {
		return nil, err
	}
	s.feed = changefeed.NewFeed(cs, d.name, s.scope)
	ce := d.settings.ChangeEvents
	if ce.FeedEnabled {
		s.poller, err = changefeed.NewPoller(changefeed.PollerDeps{Feed: s.feed,
			Monitored: d.monitored, Control: d.control, LegacyDatabaseID: d.legacyID,
			Interval: ce.FeedInterval(), Retention: ce.Retention(), Logf: d.logFn})
		if err != nil {
			return nil, err
		}
	}
	if d.settings.SLO.Enabled {
		if s.engine, err = s.newEngine(d); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *sreSignals) newEngine(d sreSignalsDeps) (*slo.Engine, error) {
	cfg := d.settings.SLO
	objs, err := sloObjectives(cfg, d.name)
	if err != nil {
		return nil, err
	}
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
		proxies = []slo.Proxy{slo.NewLatencyProxy(d.monitored, pc),
			slo.NewErrorProxy(d.monitored, d.errors, pc), slo.NewConnectionProxy(d.monitored, pc),
			slo.NewReplicationLagProxy(d.monitored, pc)}
	}
	return slo.NewEngine(slo.EngineDeps{Database: d.name, Objectives: objs, Proxies: proxies,
		Rules: rules, Store: ss, Prometheus: prom, Scope: s.scope, OnPage: s.onPage,
		Interval: cfg.EvaluationInterval(), Logf: d.logFn})
}

// probes are the signal probes every investigation collects.
func (s *sreSignals) probes() []sre.SignalProbe {
	out := []sre.SignalProbe{{ID: probes.ChangeFeed, Run: s.feed.Probe}}
	if s.engine != nil {
		out = append(out, sre.SignalProbe{ID: probes.SLOStatus, Run: s.engine.Probe})
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
// never with sre.slo.open_investigations off.
func (s *sreSignals) onPage(ctx context.Context, st slo.Status) error {
	if !s.settings.SLO.OpenInvestigations || st.BurnStartedAt == nil ||
		(st.Kind == slo.KindProxy && !s.settings.AutomaticStart) {
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
	if s.poller != nil {
		run(func() { s.poller.Run(ctx) })
	}
	if s.engine != nil {
		run(func() { s.engine.Run(ctx) })
	}
}

// logErrorCounter counts server-class errors in a database's log entries.
type logErrorCounter struct {
	sub *logwatch.EntrySubscriber
}

func (c logErrorCounter) DrainErrors() (int64, bool) {
	var n int64
	for _, e := range c.sub.Drain() {
		if slo.ServerErrorClass(e.SQLState) {
			n++
		}
	}
	return n, true
}
