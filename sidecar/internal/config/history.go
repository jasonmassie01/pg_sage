package config

import "fmt"

// HistoryConfig says where pg_sage keeps its telemetry history: the
// collector's snapshots (sage.snapshots) and the per-query samples
// (sage.query_store), the two tables that make up most of its storage.
// Findings, the action log and the verification records stay in each
// monitored database in either placement.
type HistoryConfig struct {
	Store string `yaml:"store" doc:"Where pg_sage keeps snapshots and the query store: monitored (each monitored database) or meta (the meta database; needs meta_db). Restart and history migrate to change. Default monitored."`
}

// History placements.
const (
	HistoryStoreMonitored = "monitored"
	HistoryStoreMeta      = "meta"
)

// DefaultHistory keeps history in each monitored database.
func DefaultHistory() HistoryConfig {
	return HistoryConfig{Store: HistoryStoreMonitored}
}

// HistoryInMeta reports whether history lives in the meta database.
func (c *Config) HistoryInMeta() bool {
	return c.History.Store == HistoryStoreMeta
}

// validateHistory refuses an unknown placement and a meta placement
// without a meta database. An empty value means monitored.
func (c *Config) validateHistory() error {
	switch c.History.Store {
	case "":
		c.History.Store = HistoryStoreMonitored
	case HistoryStoreMonitored:
	case HistoryStoreMeta:
		if !c.HasMetaDB() {
			return fmt.Errorf("history.store: meta keeps history in the meta database " +
				"and needs one: set meta_db (--meta-db) or use history.store: monitored")
		}
	default:
		return fmt.Errorf("history.store must be %q or %q, got %q", HistoryStoreMonitored,
			HistoryStoreMeta, c.History.Store)
	}
	return nil
}
