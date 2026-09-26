package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/agentdb"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// agentFleetPrefix namespaces agent-DB fleet instances so they never
// collide with YAML-configured databases.
const agentFleetPrefix = "agentdb:"

// eligibleForFleet reports whether an agent deployment can be monitored:
// it must be active and carry resolvable connection info.
func eligibleForFleet(dep agentdb.Deployment) bool {
	if agentDeploymentSecretRef(dep) != "" {
		_, ok := resolveAgentEnvironmentSecret(dep)
		return ok
	}
	if dep.Status != "active" {
		return false
	}
	if dep.ConnectionInfo == nil {
		return false
	}
	if _, hasSecret := dep.ConnectionInfo["secret_ref"]; hasSecret {
		return false // needs secret resolution (not yet wired)
	}
	return mapString(dep.ConnectionInfo, "host") != "" &&
		agentDBName(dep) != ""
}

// agentDeploymentToFleetConfig converts a deployment's inline connection
// info into a fleet DatabaseConfig. ok is false when the info is
// insufficient to connect.
func agentDeploymentToFleetConfig(
	dep agentdb.Deployment,
) (config.DatabaseConfig, bool) {
	if agentDeploymentSecretRef(dep) != "" {
		return resolveAgentEnvironmentSecret(dep)
	}
	if !eligibleForFleet(dep) {
		return config.DatabaseConfig{}, false
	}
	ci := dep.ConnectionInfo
	port := mapInt(ci, "port")
	if port == 0 {
		port = 5432
	}
	sslmode := mapString(ci, "sslmode")
	if sslmode == "" {
		// Agent databases are remote; never default to plaintext (G8-B21).
		sslmode = "require"
	}
	return config.DatabaseConfig{
		Name:     agentFleetPrefix + dep.DeploymentID,
		Host:     mapString(ci, "host"),
		Port:     port,
		User:     mapString(ci, "user"),
		Password: mapString(ci, "password"),
		Database: agentDBName(dep),
		SSLMode:  sslmode,
		Tags:     []string{"agentdb", dep.Provider, dep.TenantID},
	}, true
}

// agentDBName returns the connect database name for a deployment.
func agentDBName(dep agentdb.Deployment) string {
	if n := mapString(dep.ConnectionInfo, "database"); n != "" {
		return n
	}
	if n := mapString(dep.ConnectionInfo, "dbname"); n != "" {
		return n
	}
	return dep.DatabaseName
}

// syncAgentDBsToFleet reconciles the fleet with the desired set of agent
// databases: it adds eligible deployments, replaces instances whose
// connection or credentials changed, and removes instances whose deployment
// was archived, deleted, destroyed or lost its credentials (G8-B21, SURF-07).
func syncAgentDBsToFleet(
	ctx context.Context,
	store *agentdb.Store,
	mgr *fleet.DatabaseManager,
) {
	deployments, err := store.List(ctx)
	if err != nil {
		logWarn("agentdb", "fleet sync: list deployments: %v", err)
		return
	}
	current := map[string]config.DatabaseConfig{}
	for name, inst := range mgr.Instances() {
		if strings.HasPrefix(name, agentFleetPrefix) && inst != nil {
			current[name] = inst.Config
		}
	}
	add, remove := agentFleetPlan(deployments, current)
	for _, name := range remove {
		if err := mgr.RemoveInstanceContext(ctx, name); err != nil &&
			!errors.Is(err, fleet.ErrDatabaseNotFound) {
			logWarn("agentdb", "fleet sync: remove %q: %v", name, err)
			continue
		}
		logInfo("agentdb", "removed agent database %q from fleet", name)
	}
	for _, dbCfg := range add {
		connectAgentDBToFleet(ctx, mgr, dbCfg)
	}
}

// agentFleetPlan computes the fleet changes. A changed connection is a
// remove plus an add so the old pool and credentials are retired.
func agentFleetPlan(
	deployments []agentdb.Deployment,
	current map[string]config.DatabaseConfig,
) ([]config.DatabaseConfig, []string) {
	desired := map[string]config.DatabaseConfig{}
	for _, dep := range deployments {
		if dbCfg, ok := agentDeploymentToFleetConfig(dep); ok {
			desired[dbCfg.Name] = dbCfg
		}
	}
	var add []config.DatabaseConfig
	var remove []string
	for name, have := range current {
		want, ok := desired[name]
		if !ok || !sameAgentConnection(have, want) {
			remove = append(remove, name)
		}
	}
	for name, want := range desired {
		have, ok := current[name]
		if !ok || !sameAgentConnection(have, want) {
			add = append(add, want)
		}
	}
	sort.Strings(remove)
	sort.Slice(add, func(i, j int) bool { return add[i].Name < add[j].Name })
	return add, remove
}

func sameAgentConnection(a, b config.DatabaseConfig) bool {
	return a.Host == b.Host && a.Port == b.Port && a.User == b.User &&
		a.Password == b.Password && a.Database == b.Database && a.SSLMode == b.SSLMode
}

// agentDBMutationGate blocks AgentDB provider/DDL mutations while any fleet
// instance is emergency-stopped (G8-B18); the store also checks the
// persisted flag on its own pool.
func agentDBMutationGate(mgr *fleet.DatabaseManager) agentdb.MutationGate {
	return func(context.Context) error {
		if mgr == nil {
			return nil
		}
		for name, inst := range mgr.Instances() {
			if inst != nil && inst.Stopped {
				return fmt.Errorf("fleet emergency stop active on %s", name)
			}
		}
		return nil
	}
}

// connectAgentDBToFleet connects a pool for an agent database and
// registers it with a collector. The analyzer/executor pipeline is not
// yet attached for agent DBs (follow-up); this gives snapshot-level
// monitoring and fleet visibility.
func connectAgentDBToFleet(
	ctx context.Context,
	mgr *fleet.DatabaseManager,
	dbCfg config.DatabaseConfig,
) {
	if ctx != nil && ctx.Err() != nil {
		return
	}
	pool, err := connectMonitoredDB(dbCfg.ConnString(), dbCfg.MaxConnections)
	if err != nil {
		logWarn("agentdb", "fleet sync: connect %q: %v", dbCfg.Name, err)
		return
	}
	parent := agentDBRuntimeParent(ctx)
	if parent.Err() != nil {
		pool.Close()
		return
	}
	instCtx, cancel := context.WithCancel(parent)
	workers := &sync.WaitGroup{}
	dbColl := collector.New(pool, cfg, detectPGVersion(pool),
		logStructuredWrapper)
	inst := &fleet.DatabaseInstance{
		Name:      dbCfg.Name,
		Config:    dbCfg,
		Pool:      pool,
		Collector: dbColl,
		Cancel:    cancel,
		Workers:   workers,
		Status: &fleet.InstanceStatus{
			Connected:    true,
			DatabaseName: dbCfg.Database,
			LastSeen:     time.Now(),
		},
	}
	mgr.RegisterInstance(inst)
	startInstanceWorker(workers, func() { dbColl.Run(instCtx) })
	logInfo("agentdb", "registered agent database %q in fleet", dbCfg.Name)
}

func agentDBRuntimeParent(_ context.Context) context.Context {
	if shutdownCtx != nil {
		return shutdownCtx
	}
	return context.Background()
}

func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func mapInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}
