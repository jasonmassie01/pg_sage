package main

import (
	"context"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The agent kill switch, freeze and unfreeze (AGENTDB-SPEC §6.10). The API
// router is built before the governance control database is known, so it
// holds lateKillSwitch, which answers 503 until startAgentKill builds the
// switch. The kill reaches every monitored database of the fleet and each
// one's configured replicas (databases[].replicas, DSNs from environment
// variables); when the gate or the control database is down it runs the
// steps directly and audits them in agents.kill_fallback_log.

var agentKill atomic.Pointer[agentguard.Switch]

// startAgentKill builds the switch on the governance control database and
// reconciles a fallback log left by a kill that ran while it was down.
func startAgentKill(ctx context.Context, c *config.Config, mgr *fleet.DatabaseManager,
	meta *metaDBState) {
	control := governanceControlPool(c, mgr, meta)
	if control == nil || c == nil {
		return
	}
	kr, err := configSecretKeyring(ctx, control)
	if err != nil {
		logWarn("agents", "agent unfreeze cannot rotate credentials: %v", err)
	}
	kc := killConfig(c)
	sw, err := agentguard.NewSwitch(agentguard.KillDeps{Store: agentguard.NewStore(control),
		Keyring: kr, Targets: killTargets(mgr), Config: kc,
		Fallback: agentguard.NewFallbackLog(c.Agents.KillFallbackLog)})
	if err != nil {
		logError("agents", "the agent kill switch is unavailable: %v", err)
		return
	}
	agentKill.Store(sw)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if n, err := sw.ReconcileFallback(rctx); err != nil {
		logWarn("agents", "agent kill fallback log not reconciled yet: %v", err)
	} else if n > 0 {
		logInfo("agents", "reconciled %d agent kill steps from the fallback log", n)
	}
}

// agentFreezes is D1's fleet and database freeze flags for the decider.
func agentFreezes(control *pgxpool.Pool) decide.Freezes {
	sw, err := agentguard.NewSwitch(agentguard.KillDeps{Store: agentguard.NewStore(control),
		Config: agentguard.DefaultKillConfig(),
		Targets: func(context.Context) ([]agentguard.KillTarget, error) {
			return nil, nil
		}})
	if err != nil {
		return nil
	}
	return sw
}

// killConfig maps the agents settings onto the switch's bounds.
func killConfig(c *config.Config) agentguard.KillConfig {
	kc := agentguard.DefaultKillConfig()
	if c == nil {
		return kc
	}
	ms := func(v int) time.Duration { return time.Duration(v) * time.Millisecond }
	r := c.Agents.Roles
	kc.VerifyTimeout = time.Duration(c.Agents.KillVerifyTimeoutSeconds) * time.Second
	kc.SingleOperatorMode = c.Agents.SingleOperatorMode
	kc.Roles.ConnectionLimit = r.ConnectionLimit
	kc.Roles.BrokerConnectionLimit = c.Agents.Broker.PoolMaxConns
	kc.Roles.StatementTimeout = ms(r.StatementTimeoutMS)
	kc.Roles.LockTimeout = ms(r.LockTimeoutMS)
	kc.Roles.IdleInTransactionTimeout = ms(r.IdleInTransactionTimeoutMS)
	kc.Roles.IdleSessionTimeout = ms(r.IdleSessionTimeoutMS)
	kc.Roles.TransactionTimeout = ms(r.TransactionTimeoutMS)
	kc.Roles.TempFileLimitMB = r.TempFileLimitMB
	return kc
}

// killReplicas resolves each configured replica's DSN from its variable;
// an unset one stays, with no DSN, so the kill reports it unreachable.
func killReplicas(rs []config.DatabaseReplica, getenv func(string) string) []agentguard.Replica {
	out := make([]agentguard.Replica, 0, len(rs))
	for _, r := range rs {
		out = append(out, agentguard.Replica{Name: r.Name, DSN: getenv(r.DSNEnv)})
	}
	return out
}

// clusterKeys caches each pool's cluster key: the provider resource id,
// else the system identifier, else the connection target.
var clusterKeys sync.Map // *pgxpool.Pool → string

func clusterKeyOf(ctx context.Context, name string, pool *pgxpool.Pool) string {
	if k, ok := clusterKeys.Load(pool); ok {
		return k.(string)
	}
	id, err := envbind.ReadIdentity(ctx, pool, providerRefOf(name))
	key := id.ProviderRef
	if key == "" {
		key = id.SystemIdentifier
	}
	if key == "" {
		key = id.Target
	}
	if err == nil {
		clusterKeys.Store(pool, key)
	}
	return key
}

// killTargets lists the fleet's databases at kill time, by name.
func killTargets(mgr *fleet.DatabaseManager) func(context.Context) (
	[]agentguard.KillTarget, error) {
	return func(ctx context.Context) ([]agentguard.KillTarget, error) {
		if mgr == nil {
			return nil, nil
		}
		insts := mgr.Instances()
		names := make([]string, 0, len(insts))
		for name := range insts {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]agentguard.KillTarget, 0, len(names))
		for _, name := range names {
			inst := insts[name]
			if inst == nil || inst.Pool == nil {
				continue
			}
			out = append(out, killTarget(ctx, name, inst))
		}
		return out, nil
	}
}

func killTarget(ctx context.Context, name string,
	inst *fleet.DatabaseInstance) agentguard.KillTarget {
	t := agentguard.KillTarget{Name: name, Pool: inst.Pool,
		ClusterKey: clusterKeyOf(ctx, name, inst.Pool),
		Replicas:   killReplicas(inst.Config.Replicas, os.Getenv)}
	if inst.Executor != nil {
		t.Executor = inst.Executor
	}
	if inst.Investigations != nil {
		if scope, ok := inst.Investigations.Coordinator().Scope(); ok {
			t.DatabaseID = string(scope.DatabaseID)
		}
	}
	return t
}

// lateKillSwitch is the API's view of the switch: 503 until it exists.
type lateKillSwitch struct{}

func (lateKillSwitch) sw() (*agentguard.Switch, error) {
	if sw := agentKill.Load(); sw != nil {
		return sw, nil
	}
	return nil, agentguard.ErrUnavailable
}

func (l lateKillSwitch) Kill(ctx context.Context, r agentguard.KillRequest) (
	agentguard.KillReport, error) {
	sw, err := l.sw()
	if err != nil {
		return agentguard.KillReport{}, err
	}
	return sw.Kill(ctx, r)
}

func (l lateKillSwitch) Freeze(ctx context.Context, r agentguard.FreezeRequest) (
	agentguard.KillReport, error) {
	sw, err := l.sw()
	if err != nil {
		return agentguard.KillReport{}, err
	}
	return sw.Freeze(ctx, r)
}

func (l lateKillSwitch) Unfreeze(ctx context.Context, r agentguard.UnfreezeRequest) (
	agentguard.UnfreezeResult, error) {
	sw, err := l.sw()
	if err != nil {
		return agentguard.UnfreezeResult{}, err
	}
	return sw.Unfreeze(ctx, r)
}

func (l lateKillSwitch) Release(ctx context.Context, r agentguard.ReleaseRequest) (
	agentguard.UnfreezeResult, error) {
	sw, err := l.sw()
	if err != nil {
		return agentguard.UnfreezeResult{}, err
	}
	return sw.Release(ctx, r)
}
