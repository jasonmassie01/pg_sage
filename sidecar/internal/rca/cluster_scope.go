package rca

import "github.com/pg-sage/sidecar/internal/collector"

// Cluster scope (rca-08). In a fleet, several databases share one
// PostgreSQL cluster. Three snapshot signals read cluster-wide state:
// connections_high (client backends of every database against the
// cluster's max_connections), replication_lag (pg_stat_replication
// lists every walsender) and wal_growth_spike (the cluster's WAL insert
// position, rca-03). Only the cluster owner detects them, so one
// cluster event opens one incident and one page, not one per database.
// The other snapshot signals read this database's own rows (statements,
// prepared transactions since rca-01, idle sessions, vacuum, locks) and
// stay on every engine. Cluster log events are routed
// to the owner by the log fanout.

// SetClusterOwner marks whether this engine owns its cluster's
// cluster-wide signals. An engine owns them until told otherwise, so
// single-database mode is unchanged. Safe to call while cycles run.
func (e *Engine) SetClusterOwner(owner bool) {
	e.clusterNonOwner.Store(!owner)
}

// ClusterOwner reports whether the engine detects cluster-wide signals.
func (e *Engine) ClusterOwner() bool {
	return !e.clusterNonOwner.Load()
}

// connectionSlotsUsed is the client backends counted against
// max_connections: autovacuum, background workers and walsenders have
// slots of their own. A snapshot without the count (one stored before
// client_backends existed) falls back to every backend, as before.
func connectionSlotsUsed(s collector.SystemStats) int {
	if s.ClientBackends > 0 {
		return s.ClientBackends
	}
	return s.TotalBackends
}
