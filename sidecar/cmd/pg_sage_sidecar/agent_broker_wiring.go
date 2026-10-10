package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/broker"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Agent governance's brokered read path (spec §6.8): agent_query and
// agent_whoami over MCP, and an agent's activity over REST. The broker is
// built on first use, once the fleet, the environment service and the
// control database exist; without a control database or encryption_key
// the tools answer unavailable (posture-only governance).

var (
	agentBrokerMu    sync.Mutex
	agentBrokerState *agentBrokerRuntime
)

// agentBrokerRuntime is the process's broker and the stores it reads.
type agentBrokerRuntime struct {
	broker *broker.Broker
	store  *agentguard.Store
}

// agentBrokerTimeout bounds building the broker (reading the KDF salt).
const agentBrokerTimeout = 10 * time.Second

// processAgentBroker returns the broker, building it on first success.
func processAgentBroker() (*agentBrokerRuntime, error) {
	agentBrokerMu.Lock()
	defer agentBrokerMu.Unlock()
	if agentBrokerState != nil {
		return agentBrokerState, nil
	}
	ctx, cancel := context.WithTimeout(shutdownCtx, agentBrokerTimeout)
	defer cancel()
	rt, err := buildAgentBroker(ctx, cfg, fleetMgr)
	if err != nil {
		return nil, err
	}
	agentBrokerState = rt
	return rt, nil
}

func buildAgentBroker(ctx context.Context, c *config.Config,
	mgr *fleet.DatabaseManager) (*agentBrokerRuntime, error) {
	svc := agentEnvSvc
	control := governanceControlPool(c, mgr, globalMetaState)
	if c == nil || mgr == nil || svc == nil || control == nil {
		return nil, fmt.Errorf("%w: agent governance needs mode: meta or "+
			"agents.control_database", broker.ErrUnavailable)
	}
	kr, err := configSecretKeyring(ctx, control)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", broker.ErrUnavailable, err)
	}
	store := agentguard.NewStore(control)
	targets := agentTargets{svc: svc}
	b, err := broker.New(agentBrokerConfig(c), broker.Deps{Targets: targets,
		Logins:  agentLogins{store: store, kr: kr},
		Decider: sharedDecider{}, Classes: broker.StoreClasses{}, Audit: broker.SQLAudit{},
		Roles: store, Databases: func(context.Context) []string { return fleetNames(mgr) },
		TrustLevel: func() string { return c.Trust.Level },
		Log: func(level, msg string, args ...any) {
			logStructured(level, "agents", msg, args...)
		}})
	if err != nil {
		return nil, err
	}
	return &agentBrokerRuntime{broker: b, store: store}, nil
}

// agentBrokerConfig maps agents.query.*, agents.roles and agents.broker.
func agentBrokerConfig(c *config.Config) broker.Config {
	bc := broker.DefaultConfig()
	q, roles, pool := c.Agents.Query, c.Agents.Roles, c.Agents.Broker
	bc.MaxRows, bc.MaxRowsCeiling, bc.MaxBytes = q.MaxRows, q.MaxRowsCeiling, q.MaxBytes
	bc.StatementTimeout = time.Duration(roles.StatementTimeoutMS) * time.Millisecond
	bc.PoolMaxConns, bc.MaxTotalConns = pool.PoolMaxConns, pool.MaxTotalConnections
	bc.PoolIdle = time.Duration(pool.PoolIdleSeconds) * time.Second
	return bc
}

func fleetNames(mgr *fleet.DatabaseManager) []string {
	var names []string
	for name := range mgr.Instances() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// agentTargets resolves a fleet database for the broker: pg_sage's pool,
// the SRE binding's database id, the evaluated environment and the
// cluster key (provider resource id, else system identifier, §6.6).
type agentTargets struct{ svc *envbind.Service }

func (a agentTargets) Target(ctx context.Context, name string) (broker.Target, error) {
	db, err := a.svc.Resolve(ctx, name)
	if errors.Is(err, envbind.ErrUnknownDatabase) {
		return broker.Target{}, broker.ErrUnknownDatabase
	}
	if err != nil {
		return broker.Target{}, fmt.Errorf("%w: %v", broker.ErrUnavailable, err)
	}
	binding, err := a.svc.Binder.EnvironmentOf(ctx, db)
	if err != nil {
		logWarn("agents", "environment of %s not read (treated as prod): %v", name, err)
	}
	live := binding.Evidence.Live
	key := live.ProviderRef
	if key == "" {
		key = live.SystemIdentifier
	}
	return broker.Target{Name: name, DatabaseID: db.ID, Pool: db.Pool, ClusterKey: key,
		Env: binding.Env, Verified: binding.Evidence.Verified}, nil
}

// agentObjectChecker is D5 for brokered requests: the broker's minimal
// catalog checker until the grants workstream's checker is wired.
func agentObjectChecker(svc *envbind.Service) decide.ObjectChecker {
	return broker.CatalogObjects{Targets: agentTargets{svc: svc},
		Classes: broker.StoreClasses{}}
}

// sharedDecider is the gate's process decider (agent_gate_wiring.go), read
// at decision time; before governance starts every agent read is refused.
type sharedDecider struct{}

func (sharedDecider) Decide(ctx context.Context, req decide.Request) decide.Verdict {
	state := agentGate.Load()
	if state == nil {
		return decide.Verdict{Reason: decide.ReasonUnavailable, Step: "D1",
			Detail: "agent governance has not started (no control database)",
			Fix:    "set agents.control_database or use mode: meta"}
	}
	return state.decider.Decide(ctx, req)
}
