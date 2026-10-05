package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/selfconfig"
	"github.com/pg-sage/sidecar/internal/store"
)

// Self-configuration (roadmap phase 3): each database derives its unset
// derivable settings from evidence at startup, before any worker reads
// them, and re-derives them on a timer (live keys adopt promoted values at
// once; restart-bound keys are reported pending restart).

// selfConfigInterval is the period of the live derivation pass.
var selfConfigInterval = time.Hour

// selfConfigStartupBudget bounds the startup pass (evidence and ledger).
const selfConfigStartupBudget = time.Minute

type selfConfigRunner struct {
	name        string
	databaseID  int
	pool        *pgxpool.Pool // the monitored database (ledger and evidence)
	controlPool *pgxpool.Pool // where API overrides live; nil: none
	configPath  string
	cfg         *config.Config // the runtime config derived values go into
	operator    func() *config.Config
	engine      *selfconfig.Engine
	gather      func(context.Context) (selfconfig.Evidence, error)
	prev        selfconfig.Evidence
	logInfo     func(component, msg string, args ...any)
	logWarn     func(component, msg string, args ...any)
}

func (rt *databaseRuntime) newSelfConfigRunner() *selfConfigRunner {
	pool := rt.spec.Pool
	return &selfConfigRunner{
		name: rt.spec.Name, databaseID: rt.spec.DatabaseID, pool: pool,
		controlPool: configControlPool(), configPath: rt.cfg.ConfigPath, cfg: rt.cfg,
		operator: desiredOperatorConfig,
		engine:   selfconfig.NewEngine(selfconfig.NewStore(pool)),
		gather: func(ctx context.Context) (selfconfig.Evidence, error) {
			return selfconfig.Gather(ctx, pool)
		},
		logInfo: logInfo, logWarn: logWarn,
	}
}

// desiredOperatorConfig is the configuration as the operator declared it
// (file and overrides, never derived values); nil before the controller.
func desiredOperatorConfig() *config.Config {
	if configController == nil {
		return nil
	}
	return configController.Desired().Config
}

// deriveSettingsAtStartup runs the startup pass on the runtime config.
func (rt *databaseRuntime) deriveSettingsAtStartup() {
	rt.newSelfConfigRunner().deriveAtStartup(rt.ctx)
}

// startSelfConfig runs the live pass on the instance's worker group.
func (rt *databaseRuntime) startSelfConfig() {
	if !rt.cfg.SelfConfig.Enabled {
		return
	}
	r := rt.newSelfConfigRunner()
	rt.start(func() { r.loop(rt.ctx, selfConfigInterval) })
	rt.note("self_config")
}

func (s *selfConfigRunner) deriveAtStartup(ctx context.Context) {
	if !s.cfg.SelfConfig.Enabled {
		s.logInfo("selfconfig", "db %q: derived settings off (self_config.enabled: false)",
			s.name)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, selfConfigStartupBudget)
	defer cancel()
	results, err := s.pass(ctx, selfconfig.PhaseStartup)
	if err != nil {
		s.logWarn("selfconfig", "db %q: derived settings not applied (defaults and the "+
			"operator's values stay): %v", s.name, err)
		return
	}
	s.logInfo("selfconfig", "db %q: derived settings: %s", s.name,
		selfconfig.Summary(results))
}

func (s *selfConfigRunner) loop(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			results, err := s.pass(ctx, selfconfig.PhaseLive)
			if err != nil {
				s.logWarn("selfconfig", "db %q: derivation pass: %v", s.name, err)
				continue
			}
			s.logEvents(results)
		}
	}
}

func (s *selfConfigRunner) logEvents(results []selfconfig.Result) {
	for _, r := range results {
		for _, e := range r.Events {
			value := "-"
			if e.Value != nil {
				value = fmt.Sprintf("%g", *e.Value)
			}
			s.logInfo("selfconfig", "db %q: %s %s=%s: %s", s.name, e.Kind, r.Key, value,
				e.Reason)
		}
	}
}

// pass derives once. It fails closed when the operator-set keys cannot be
// read; evidence that fails to read is logged and the rest is used.
func (s *selfConfigRunner) pass(ctx context.Context,
	phase selfconfig.Phase) ([]selfconfig.Result, error) {
	set, err := s.operatorSet(ctx)
	if err != nil {
		return nil, fmt.Errorf("read which keys the operator set: %w", err)
	}
	ev, gerr := s.gather(ctx)
	if gerr != nil {
		s.logWarn("selfconfig", "db %q: some evidence could not be read: %v", s.name, gerr)
	}
	in := selfconfig.Input{Cfg: s.cfg, OperatorSet: set, Phase: phase,
		Evidence: ev.WithTempRateSince(s.prev)}
	if s.operator != nil {
		in.Operator = s.operator()
	}
	results, err := s.engine.Reconcile(ctx, in)
	if err != nil {
		return nil, err
	}
	if ev.TempBytes.Known {
		s.prev = ev
	}
	return results, nil
}

// operatorSet is every key the operator set for this database.
func (s *selfConfigRunner) operatorSet(ctx context.Context) (map[string]bool, error) {
	return operatorSetKeys(ctx, s.configPath, s.name, s.controlPool, s.databaseID)
}

// operatorSetKeys is every key the operator set for database name: the
// YAML file at configPath (with the fleet aliases) and the API overrides in
// controlPool (nil: none), global and the database's own (databaseID > 0).
func operatorSetKeys(ctx context.Context, configPath, name string,
	controlPool *pgxpool.Pool, databaseID int) (map[string]bool, error) {
	set, err := config.OperatorSetPathsFromFile(configPath, name)
	if err != nil {
		return nil, err
	}
	if controlPool == nil {
		return set, nil
	}
	cs := store.NewConfigStore(controlPool)
	ids := []int{0}
	if databaseID > 0 {
		ids = append(ids, databaseID)
	}
	for _, id := range ids {
		overrides, err := cs.GetOverrides(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("read config overrides: %w", err)
		}
		for _, o := range overrides {
			set[o.Key] = true
		}
	}
	return set, nil
}
