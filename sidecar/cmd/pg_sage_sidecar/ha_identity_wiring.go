package main

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/ha"
)

// newPersistedHAMonitor is the earned-autonomy HA monitor of one
// database. It persists the node's identity in the control database, so
// a failover that happened while pg_sage was down still opens the
// failover cooldown at startup (Sage SRE follow-ups B).
func newPersistedHAMonitor(b autonomyBinding) *ha.Monitor {
	return ha.New(b.monitored, logStructuredWrapper).WithIdentityStore(
		ha.NewPostgresIdentityStore(b.control), haIdentityKey(b.database, b.databaseID))
}

// haIdentityKey is the stable history key of a monitored database: its
// meta-db record id when there is one, otherwise its instance name ("" when
// it has neither, which leaves persistence off).
func haIdentityKey(database string, databaseID *int) string {
	switch {
	case databaseID != nil:
		return fmt.Sprintf("db:%d", *databaseID)
	case database != "":
		return "name:" + database
	}
	return ""
}
