package migration

import (
	"context"
	"sync"
)

// repackAlternatives are the safe alternatives that name pg_repack. They
// replace the rule's own template only when the server offers the
// extension: recommending a tool the operator cannot install (on a
// managed service without it, say) is advice they cannot act on.
var repackAlternatives = map[string]string{
	"ddl_vacuum_full": "Use pg_repack or regular VACUUM instead",
	"ddl_cluster":     "Schedule during maintenance window or use pg_repack",
}

// repackProbeSQL asks whether pg_repack can be installed on the server.
// pg_available_extensions is readable by any role on PostgreSQL 14-18 and
// on the managed services pg_sage supports.
const repackProbeSQL = `/* pg_sage */ SELECT EXISTS (
    SELECT 1 FROM pg_available_extensions WHERE name = 'pg_repack')`

// repackCache remembers the server's answer. A failed probe is not
// cached, so the next assessment asks again; until an answer arrives
// pg_repack is not recommended.
type repackCache struct {
	mu        sync.Mutex
	known     bool
	available bool
	probe     func(context.Context) (bool, error)
}

func newRepackCache(probe func(context.Context) (bool, error)) *repackCache {
	return &repackCache{probe: probe}
}

// availableOn reports whether pg_repack is available, probing at most
// once per success. The lock is held across the probe so parallel
// assessments (activity poller and log detector) share one query.
func (c *repackCache) availableOn(
	ctx context.Context, logFn func(string, string, ...any),
) bool {
	if c == nil || c.probe == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known {
		return c.available
	}
	ok, err := c.probe(ctx)
	if err != nil {
		logFn("warn", "migration: pg_repack availability check failed "+
			"(advice omits pg_repack; retrying on the next assessment): %v", err)
		return false
	}
	c.known, c.available = true, ok
	return ok
}

// probeRepack runs repackProbeSQL as a bounded catalog read.
func (ra *RiskAssessor) probeRepack(ctx context.Context) (bool, error) {
	var ok bool
	err := ra.catalog().QueryRow(ctx, repackProbeSQL).Scan(&ok)
	return ok, err
}

// safeAlternative is the classification's alternative, naming pg_repack
// for VACUUM FULL and CLUSTER only when the server offers it.
func (ra *RiskAssessor) safeAlternative(
	ctx context.Context, c DDLClassification,
) string {
	alt, mentionsRepack := repackAlternatives[c.RuleID]
	if !mentionsRepack || !ra.repack.availableOn(ctx, ra.logFn) {
		return c.SafeAlternative
	}
	return alt
}
