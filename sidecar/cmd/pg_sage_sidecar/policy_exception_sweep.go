package main

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// policyExceptionSweepInterval is how often expired policy exceptions are
// reverted to their set (fleetpol-01a). The gate already ignores an
// expired exception; the sweep supersedes its document and audits it.
const policyExceptionSweepInterval = time.Minute

// startPolicyExceptionSweep reverts expired exceptions in the control
// database every minute until ctx (the process shutdown context) ends.
func startPolicyExceptionSweep(ctx context.Context, control *pgxpool.Pool) {
	if control == nil {
		return
	}
	store := policy.NewStore(control)
	go func() {
		ticker := time.NewTicker(policyExceptionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepPolicyExceptions(ctx, store)
			}
		}
	}()
}

func sweepPolicyExceptions(ctx context.Context, store *policy.Store) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n, err := store.ExpireExceptions(ctx)
	if err != nil {
		logWarn("policy", "revert expired policy exceptions: %v; the gate already "+
			"ignores them, the next sweep retries", err)
		return
	}
	if n > 0 {
		logInfo("policy", "%d expired policy exception(s) reverted to their set", n)
	}
}

// fleetTagMap turns the fleet's "key=value" tags into the map
// sage.databases.tags holds and policy-set selectors match; a bare tag
// has the value "".
func fleetTagMap(tags []string) map[string]string {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		key, value, _ := strings.Cut(tag, "=")
		if key = strings.TrimSpace(key); key != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}
