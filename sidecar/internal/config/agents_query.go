package config

import "fmt"

// AgentsQueryConfig bounds agent_query, the brokered read path
// (spec §6.8, §9). Its statement timeout is
// agents.roles.statement_timeout_ms, set in every brokered transaction.
type AgentsQueryConfig struct {
	MaxRows        int `yaml:"max_rows" doc:"Rows agent_query returns when the agent asks for no bound; it fetches one more only to report truncation. At least 1 and at most max_rows_ceiling. Default: 200."`
	MaxRowsCeiling int `yaml:"max_rows_ceiling" doc:"Largest max_rows an agent may ask agent_query for. At most 100000. Default: 1000."`
	MaxBytes       int `yaml:"max_bytes" doc:"Bytes of row values one agent_query result may carry; the rows that fit are returned and the result is marked truncated. 1 to 1073741824. Default: 1048576."`
	// AuditRetentionDays ages out sage.guard_query_audit (spec §6.17).
	AuditRetentionDays int `yaml:"audit_retention_days" doc:"Days to keep the agent_query audit (sage.guard_query_audit, in each monitored database). 0 keeps it forever. Range 0-3650. Default: 30."`
}

// agent_query defaults (spec §9).
const (
	DefaultAgentQueryMaxRows        = 200
	DefaultAgentQueryMaxRowsCeiling = 1000
	DefaultAgentQueryMaxBytes       = 1 << 20
	maxAgentQueryRowsCeiling        = 100000
	maxAgentQueryBytes              = 1 << 30
	DefaultAgentQueryAuditDays      = 30
	maxAgentQueryAuditDays          = 3650
)

func defaultAgentsQuery() AgentsQueryConfig {
	return AgentsQueryConfig{MaxRows: DefaultAgentQueryMaxRows,
		MaxRowsCeiling: DefaultAgentQueryMaxRowsCeiling,
		MaxBytes:       DefaultAgentQueryMaxBytes, AuditRetentionDays: DefaultAgentQueryAuditDays}
}

// validateQuery checks the agent_query bounds.
func (a AgentsConfig) validateQuery() error {
	q := a.Query
	switch {
	case q.MaxRows < 1:
		return fmt.Errorf("agents.query.max_rows must be at least 1, got %d", q.MaxRows)
	case q.MaxRowsCeiling < 1 || q.MaxRowsCeiling > maxAgentQueryRowsCeiling:
		return fmt.Errorf("agents.query.max_rows_ceiling must be 1 to %d, got %d",
			maxAgentQueryRowsCeiling, q.MaxRowsCeiling)
	case q.MaxRows > q.MaxRowsCeiling:
		return fmt.Errorf("agents.query.max_rows (%d) must be at most "+
			"agents.query.max_rows_ceiling (%d)", q.MaxRows, q.MaxRowsCeiling)
	case q.MaxBytes < 1 || q.MaxBytes > maxAgentQueryBytes:
		return fmt.Errorf("agents.query.max_bytes must be 1 to %d, got %d",
			maxAgentQueryBytes, q.MaxBytes)
	case q.AuditRetentionDays < 0 || q.AuditRetentionDays > maxAgentQueryAuditDays:
		return fmt.Errorf("agents.query.audit_retention_days must be 0 to %d, got %d",
			maxAgentQueryAuditDays, q.AuditRetentionDays)
	}
	return nil
}
