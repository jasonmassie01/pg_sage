package main

import (
	"sync"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/pgaudit"
)

// pgauditRoles is the process's one agent-role cache: every database's
// correlator resolves roles through the same control database.
var pgauditRoles struct {
	once  sync.Once
	cache *pgaudit.RoleCache
}

// pgauditRoleCache is the shared role cache, built on first use. A failed
// read of the roles is logged once per failing streak.
func pgauditRoleCache() *pgaudit.RoleCache {
	pgauditRoles.once.Do(func() {
		source := pgauditRoleSource(func() (*config.Config, *fleet.DatabaseManager,
			*metaDBState) {
			return cfg, fleetMgr, globalMetaState
		})
		pgauditRoles.cache = pgaudit.NewRoleCache(source, func(err error) {
			logWarn("pgaudit", "agent roles unreadable: slug claims and direct agent "+
				"sessions are stored unverified until the read succeeds: %v", err)
		})
	})
	return pgauditRoles.cache
}

// pgauditRoleSource is agent governance's control database at each read,
// so one registered after the first correlator starts is still found; no
// control database is no querier (never a typed nil pool). It logs
// nothing: governance reports its own posture once the fleet is wired.
func pgauditRoleSource(state func() (*config.Config, *fleet.DatabaseManager,
	*metaDBState)) func() pgaudit.RoleQuerier {
	return func() pgaudit.RoleQuerier {
		c, mgr, meta := state()
		if p := controlPoolOf(c, mgr, meta); p != nil {
			return p
		}
		return nil
	}
}
