package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
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
}

// newSREInvestigator builds one database's investigator (coordinator,
// trigger adapter and read service) and binds its database identity.
func newSREInvestigator(d sreInvestigatorDeps) (*sre.Service, error) {
	store, err := sre.NewPostgresStore(d.control, sre.DefaultLimits())
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
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: store, Runner: d.runner,
		Triggers: sre.NewPGTriggerSource(d.monitored, d.name), Config: cc,
		LogFn: d.logFn})
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
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: rt.spec.ControlPool,
		monitored: rt.spec.Pool, runner: rt.probes, name: rt.spec.Name,
		runtimeKey: key, legacyID: legacy, settings: rt.cfg.SRE,
		logFn: logStructuredWrapper})
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: sre investigator not started: %v", rt.spec.Name, err)
		return
	}
	rt.sreService, rt.sre = svc, svc.Coordinator()
	rt.start(func() { rt.sre.Run(rt.ctx) })
	rt.sreStarted = true
	rt.note("sre_investigator")
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
