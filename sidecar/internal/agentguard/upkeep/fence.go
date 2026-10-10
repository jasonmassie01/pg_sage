package upkeep

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/agentguard"
)

// Fence is the leader lease a pass writes under (v2.3
// leader.Elector.Fence). An empty Holder writes unfenced (no election).
type Fence struct {
	Scope  string
	Holder string
	Epoch  int64
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const fenceSQL = `/* pg_sage agent_upkeep_fence v1 */ SELECT 1 FROM sage.fleet_leader_lease
	WHERE scope = $1 AND holder = $2 AND epoch = $3 AND expires_at > now()`

// check is ErrFenced unless the lease is still this holder's at this epoch.
func (f Fence) check(ctx context.Context, q rowQuerier) error {
	if f.Holder == "" {
		return nil
	}
	var one int
	err := q.QueryRow(ctx, fenceSQL, f.Scope, f.Holder, f.Epoch).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return fmt.Errorf("upkeep: check leader lease: %w", err)
	}
	return nil
}

// clusterFor builds the cluster of key from the fleet's databases: every
// database of the cluster with a pool, administered through the first one
// (by name) with an executor. ok is false when no database of the cluster
// has an executor to run a role contract.
func clusterFor(targets []agentguard.KillTarget, key string) (agentguard.Cluster,
	agentguard.Applier, bool) {
	if key == "" {
		return agentguard.Cluster{}, nil, false
	}
	var in []agentguard.KillTarget
	for _, t := range targets {
		if t.ClusterKey == key && t.Pool != nil && t.Name != "" {
			in = append(in, t)
		}
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Name < in[j].Name })
	c := agentguard.Cluster{Key: key}
	var ex agentguard.Applier
	for _, t := range in {
		c.Databases = append(c.Databases, agentguard.ClusterDatabase{Name: t.Name,
			Pool: t.Pool})
		if ex == nil && t.Executor != nil {
			c.Admin, ex = t.Pool, t.Executor
		}
	}
	return c, ex, ex != nil
}

// groupClusters maps each cluster key to its databases' targets, in name
// order; targets without a key are returned apart.
func groupClusters(targets []agentguard.KillTarget) (map[string][]agentguard.KillTarget,
	[]agentguard.KillTarget) {
	groups := map[string][]agentguard.KillTarget{}
	var noKey []agentguard.KillTarget
	for _, t := range targets {
		if t.Pool == nil {
			continue
		}
		if t.ClusterKey == "" {
			noKey = append(noKey, t)
			continue
		}
		groups[t.ClusterKey] = append(groups[t.ClusterKey], t)
	}
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].Name < g[j].Name })
	}
	return groups, noKey
}
