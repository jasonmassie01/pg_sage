package config

import (
	"fmt"
	"regexp"
)

// AgentsRolesConfig is the attributes and per-database settings of agent
// roles (AGENTDB-SPEC §6.6, §9). The timeouts also bound sessions a kill
// cannot reach (an unconfigured standby).
type AgentsRolesConfig struct {
	ConnectionLimit            int `yaml:"connection_limit" doc:"CONNECTION LIMIT of each agent's direct-lane role. At least 1. Default: 5."`
	StatementTimeoutMS         int `yaml:"statement_timeout_ms" doc:"statement_timeout set on agent roles in each database, in milliseconds. Greater than 0. Default: 30000."`
	LockTimeoutMS              int `yaml:"lock_timeout_ms" doc:"lock_timeout set on agent roles in each database, in milliseconds. Greater than 0. Default: 2000."`
	IdleInTransactionTimeoutMS int `yaml:"idle_in_transaction_timeout_ms" doc:"idle_in_transaction_session_timeout set on agent roles, in milliseconds. Greater than 0. Default: 60000."`
	IdleSessionTimeoutMS       int `yaml:"idle_session_timeout_ms" doc:"idle_session_timeout set on agent roles, in milliseconds; it bounds sessions a kill cannot reach. Greater than 0. Default: 600000."`
	TransactionTimeoutMS       int `yaml:"transaction_timeout_ms" doc:"transaction_timeout set on agent roles on PostgreSQL 17 and later, in milliseconds. Greater than 0. Default: 600000."`
	TempFileLimitMB            int `yaml:"temp_file_limit_mb" doc:"temp_file_limit set on agent roles where pg_sage may set it (it is superuser-only unless granted); 0 sets none. Default: 1024."`
	RetireGraceDays            int `yaml:"retire_grace_days" doc:"Days a retired agent's roles are kept before they are dropped. 0 or more. Default: 7."`
}

// AgentsBrokerConfig is the brokered lane's login and pool (§6.4, §9).
type AgentsBrokerConfig struct {
	RotationDays        int `yaml:"rotation_days" doc:"Days between rotations of each agent's broker password; it also rotates at every unfreeze after a kill. At least 1. Default: 7."`
	PoolMaxConns        int `yaml:"pool_max_conns" doc:"Connections per agent broker pool, and the broker role's CONNECTION LIMIT. At least 1. Default: 2."`
	PoolIdleSeconds     int `yaml:"pool_idle_seconds" doc:"Seconds an idle broker connection is kept. At least 1. Default: 60."`
	MaxTotalConnections int `yaml:"max_total_connections" doc:"Broker connections across every agent, per sidecar. At least pool_max_conns. Default: 20."`
}

// Agent role and broker defaults (spec §9).
const (
	DefaultAgentConnectionLimit      = 5
	DefaultAgentStatementTimeoutMS   = 30000
	DefaultAgentLockTimeoutMS        = 2000
	DefaultAgentIdleInTxTimeoutMS    = 60000
	DefaultAgentIdleSessionTimeoutMS = 600000
	DefaultAgentTxTimeoutMS          = 600000
	DefaultAgentTempFileLimitMB      = 1024
	DefaultAgentRetireGraceDays      = 7
	DefaultBrokerRotationDays        = 7
	DefaultBrokerPoolMaxConns        = 2
	DefaultBrokerPoolIdleSeconds     = 60
	DefaultBrokerMaxTotalConnections = 20
)

func defaultAgentsRoles() AgentsRolesConfig {
	return AgentsRolesConfig{ConnectionLimit: DefaultAgentConnectionLimit,
		StatementTimeoutMS:         DefaultAgentStatementTimeoutMS,
		LockTimeoutMS:              DefaultAgentLockTimeoutMS,
		IdleInTransactionTimeoutMS: DefaultAgentIdleInTxTimeoutMS,
		IdleSessionTimeoutMS:       DefaultAgentIdleSessionTimeoutMS,
		TransactionTimeoutMS:       DefaultAgentTxTimeoutMS,
		TempFileLimitMB:            DefaultAgentTempFileLimitMB,
		RetireGraceDays:            DefaultAgentRetireGraceDays}
}

func defaultAgentsBroker() AgentsBrokerConfig {
	return AgentsBrokerConfig{RotationDays: DefaultBrokerRotationDays,
		PoolMaxConns: DefaultBrokerPoolMaxConns, PoolIdleSeconds: DefaultBrokerPoolIdleSeconds,
		MaxTotalConnections: DefaultBrokerMaxTotalConnections}
}

// validateCore checks the role and broker settings.
func (a AgentsConfig) validateCore() error {
	r, b := a.Roles, a.Broker
	positive := []struct {
		key   string
		value int
	}{
		{"agents.roles.connection_limit", r.ConnectionLimit},
		{"agents.roles.statement_timeout_ms", r.StatementTimeoutMS},
		{"agents.roles.lock_timeout_ms", r.LockTimeoutMS},
		{"agents.roles.idle_in_transaction_timeout_ms", r.IdleInTransactionTimeoutMS},
		{"agents.roles.idle_session_timeout_ms", r.IdleSessionTimeoutMS},
		{"agents.roles.transaction_timeout_ms", r.TransactionTimeoutMS},
		{"agents.broker.rotation_days", b.RotationDays},
		{"agents.broker.pool_max_conns", b.PoolMaxConns},
		{"agents.broker.pool_idle_seconds", b.PoolIdleSeconds},
	}
	for _, p := range positive {
		if p.value < 1 {
			return fmt.Errorf("%s must be at least 1, got %d", p.key, p.value)
		}
	}
	switch {
	case r.TempFileLimitMB < 0:
		return fmt.Errorf("agents.roles.temp_file_limit_mb must not be negative, got %d",
			r.TempFileLimitMB)
	case r.RetireGraceDays < 0:
		return fmt.Errorf("agents.roles.retire_grace_days must not be negative, got %d",
			r.RetireGraceDays)
	case b.MaxTotalConnections < b.PoolMaxConns:
		return fmt.Errorf("agents.broker.max_total_connections (%d) must be at least "+
			"agents.broker.pool_max_conns (%d)", b.MaxTotalConnections, b.PoolMaxConns)
	}
	return nil
}

// ValidStdioPrincipal checks mcp.stdio_principal: empty, or a principal
// name (^[a-z][a-z0-9-]{1,62}$). Whether it exists is checked at startup.
func ValidStdioPrincipal(name string) error {
	if name == "" || stdioPrincipalPattern.MatchString(name) {
		return nil
	}
	return fmt.Errorf("mcp.stdio_principal %q must be an agent name "+
		"(lowercase letters, digits and dashes, starting with a letter)", name)
}

var stdioPrincipalPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
