package main

import (
	"context"
	"fmt"
	"strconv"
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
// it must be active and carry inline connection info (host + database).
// Cloud deployments whose credentials live behind an unresolved
// secret_ref are skipped here until secret resolution is wired (B1).
func eligibleForFleet(dep agentdb.Deployment) bool {
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
		sslmode = "disable"
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

// syncAgentDBsToFleet registers eligible agent-provisioned databases into
// the fleet so the collector monitors them. It connects new ones and
// skips any already registered. Dormant when no agent DBs exist (B1).
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
	for _, dep := range deployments {
		dbCfg, ok := agentDeploymentToFleetConfig(dep)
		if !ok {
			continue
		}
		if mgr.GetInstance(dbCfg.Name) != nil {
			continue // already registered
		}
		connectAgentDBToFleet(ctx, mgr, dbCfg)
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
	pool, err := connectMonitoredDB(dbCfg.ConnString(), dbCfg.MaxConnections)
	if err != nil {
		logWarn("agentdb", "fleet sync: connect %q: %v", dbCfg.Name, err)
		return
	}
	instCtx, cancel := context.WithCancel(ctx)
	inst := &fleet.DatabaseInstance{
		Name:   dbCfg.Name,
		Config: dbCfg,
		Pool:   pool,
		Cancel: cancel,
		Status: &fleet.InstanceStatus{
			Connected:    true,
			DatabaseName: dbCfg.Database,
			LastSeen:     time.Now(),
		},
	}
	mgr.RegisterInstance(inst)

	dbColl := collector.New(pool, cfg, detectPGVersion(pool),
		logStructuredWrapper)
	go dbColl.Run(instCtx)
	logInfo("agentdb", "registered agent database %q in fleet", dbCfg.Name)
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
