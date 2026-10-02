package main

import (
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// newSignals builds the database's M5 signals (change feed and SLO
// engine), or nil when they cannot be built: investigations then keep
// their M2 plans.
func (rt *databaseRuntime) newSignals(legacyID *int) *sreSignals {
	sig, err := newSRESignals(sreSignalsDeps{control: rt.spec.ControlPool,
		monitored: rt.spec.Pool, name: rt.spec.Name, legacyID: legacyID,
		settings: rt.cfg.SRE, errors: rt.logErrorCounter(), logFn: logStructuredWrapper})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: sre change feed and SLOs not started: %v",
			rt.spec.Name, err)
		return nil
	}
	return sig
}

// logErrorCounter subscribes the error-class proxy to this database's
// log entries when a log watcher runs; nil leaves the proxy unknown.
func (rt *databaseRuntime) logErrorCounter() slo.ErrorCounter {
	slos := rt.cfg.SRE.SLO
	if rt.logFanout == nil || !slos.Enabled || !slos.Proxies.Enabled {
		return nil
	}
	sub := rt.logFanout.SubscribeEntries("sre-slo:"+rt.spec.Name, rt.spec.Name)
	rt.start(func() {
		<-rt.ctx.Done()
		sub.Stop()
	})
	return logErrorCounter{sub: sub}
}

// startSignals binds the signals to the investigator and runs them.
func (rt *databaseRuntime) startSignals(sig *sreSignals) {
	if sig == nil || rt.sre == nil {
		return
	}
	sig.attach(rt.sre)
	rt.changeFeed, rt.sloEngine = sig.feed, sig.engine
	sig.start(rt.start, rt.ctx)
	if sig.poller != nil {
		rt.note("sre_change_feed")
	}
	if sig.engine != nil {
		rt.note("sre_slo")
	}
}
