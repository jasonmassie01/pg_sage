package config

import (
	"strings"
	"testing"
)

// agents.roles.*, agents.broker.*, agents.single_operator_mode and
// mcp.stdio_principal: the G1 agent governance core (spec §9).

func TestAgentsCoreDefaults(t *testing.T) {
	c := DefaultConfig()
	r, b := c.Agents.Roles, c.Agents.Broker
	want := AgentsRolesConfig{ConnectionLimit: 5, StatementTimeoutMS: 30000,
		LockTimeoutMS: 2000, IdleInTransactionTimeoutMS: 60000,
		IdleSessionTimeoutMS: 600000, TransactionTimeoutMS: 600000, TempFileLimitMB: 1024,
		RetireGraceDays: 7}
	if r != want {
		t.Fatalf("roles = %+v, want %+v", r, want)
	}
	if b != (AgentsBrokerConfig{RotationDays: 7, PoolMaxConns: 2, PoolIdleSeconds: 60,
		MaxTotalConnections: 20}) {
		t.Fatalf("broker = %+v", b)
	}
	if c.Agents.SingleOperatorMode || c.MCP.StdioPrincipal != "" {
		t.Fatalf("single_operator_mode %v stdio_principal %q", c.Agents.SingleOperatorMode,
			c.MCP.StdioPrincipal)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
}

// Default value masking: a partial agents block keeps every other default.
func TestAgentsCorePartialYAMLKeepsDefaults(t *testing.T) {
	path := wave5WriteYAML(t, "agents:\n  roles:\n    statement_timeout_ms: 5000\n"+
		"  broker:\n    rotation_days: 1\n  single_operator_mode: true\n"+
		"mcp:\n  stdio_principal: lifeos-claude\n")
	c := DefaultConfig()
	if err := loadYAML(path, c); err != nil {
		t.Fatal(err)
	}
	r, b := c.Agents.Roles, c.Agents.Broker
	if r.StatementTimeoutMS != 5000 || r.LockTimeoutMS != 2000 || r.ConnectionLimit != 5 {
		t.Fatalf("roles = %+v", r)
	}
	if b.RotationDays != 1 || b.PoolMaxConns != 2 || b.MaxTotalConnections != 20 {
		t.Fatalf("broker = %+v", b)
	}
	if !c.Agents.SingleOperatorMode || c.MCP.StdioPrincipal != "lifeos-claude" {
		t.Fatalf("loaded %+v %q", c.Agents, c.MCP.StdioPrincipal)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestAgentsCoreValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"zero connection limit": {func(c *Config) { c.Agents.Roles.ConnectionLimit = 0 },
			"agents.roles.connection_limit"},
		"zero statement timeout": {func(c *Config) { c.Agents.Roles.StatementTimeoutMS = 0 },
			"agents.roles.statement_timeout_ms"},
		"negative lock timeout": {func(c *Config) { c.Agents.Roles.LockTimeoutMS = -1 },
			"agents.roles.lock_timeout_ms"},
		"zero idle tx": {func(c *Config) { c.Agents.Roles.IdleInTransactionTimeoutMS = 0 },
			"idle_in_transaction_timeout_ms"},
		"zero idle session": {func(c *Config) { c.Agents.Roles.IdleSessionTimeoutMS = 0 },
			"idle_session_timeout_ms"},
		"zero tx timeout": {func(c *Config) { c.Agents.Roles.TransactionTimeoutMS = 0 },
			"transaction_timeout_ms"},
		"negative temp": {func(c *Config) { c.Agents.Roles.TempFileLimitMB = -1 },
			"temp_file_limit_mb"},
		"negative grace": {func(c *Config) { c.Agents.Roles.RetireGraceDays = -1 },
			"retire_grace_days"},
		"zero rotation": {func(c *Config) { c.Agents.Broker.RotationDays = 0 },
			"agents.broker.rotation_days"},
		"zero pool": {func(c *Config) { c.Agents.Broker.PoolMaxConns = 0 },
			"agents.broker.pool_max_conns"},
		"zero idle": {func(c *Config) { c.Agents.Broker.PoolIdleSeconds = 0 },
			"pool_idle_seconds"},
		"total below pool": {func(c *Config) { c.Agents.Broker.MaxTotalConnections = 1 },
			"max_total_connections"},
		"bad stdio principal": {func(c *Config) { c.MCP.StdioPrincipal = "Claude Code" },
			"mcp.stdio_principal"},
		"one char stdio": {func(c *Config) { c.MCP.StdioPrincipal = "c" },
			"mcp.stdio_principal"},
	}
	for name, tc := range cases {
		c := DefaultConfig()
		tc.mutate(c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want mention of %s", name, err, tc.want)
		}
	}
}

func TestAgentsCoreBoundariesAccepted(t *testing.T) {
	c := DefaultConfig()
	c.Agents.Roles.TempFileLimitMB = 0 // sets none
	c.Agents.Roles.RetireGraceDays = 0
	c.Agents.Roles.ConnectionLimit = 1
	c.Agents.Broker.MaxTotalConnections = c.Agents.Broker.PoolMaxConns
	c.MCP.StdioPrincipal = "ab"
	if err := c.Validate(); err != nil {
		t.Fatalf("boundaries rejected: %v", err)
	}
}
