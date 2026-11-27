package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// Cluster references (forecast-09). In fleet mode one owner database per
// cluster reports the cluster's runways and forecasts (slots, disk, xid
// age, connections, checkpoints) in its own sage.findings. A list of a
// non-owner's findings shows the owner's open cluster findings as
// read-only references under cluster_refs: the page, its total and its
// cursor stay the database's own, and no case or action is taken from a
// reference (act on it in the owner's database).

// clusterRefsLimit bounds the references one list adds per owner.
const clusterRefsLimit = 50

// clusterRefsSQL reads another database's open cluster findings that
// name database among their cluster's databases and that database's
// owner as the reporter.
const clusterRefsSQL = findingsColumns + ` FROM sage.findings
	WHERE status = 'open' AND detail ->> 'scope' = 'cluster'
	  AND detail ->> 'owner_database' = $1
	  AND jsonb_typeof(detail -> 'cluster_databases') = 'array'
	  AND detail -> 'cluster_databases' ? $2
	ORDER BY id LIMIT $3`

// clusterRefs lists the open cluster findings other fleet databases own
// for database's cluster. One fleet database (or "all") has none. An
// unreadable database is logged and skipped: references are a view, the
// database's own findings are the answer.
func clusterRefs(ctx context.Context, mgr *fleet.DatabaseManager,
	database string) []map[string]any {
	refs := []map[string]any{}
	if database == "all" || mgr == nil || mgr.InstanceCount() < 2 {
		return refs
	}
	for _, s := range poolsForDatabaseSelection(mgr, "all") {
		if s.name == database {
			continue
		}
		rows, err := ownerClusterRefs(ctx, s, database)
		if err != nil {
			slog.Warn("findings: cluster references skipped", "database", database,
				"owner", s.name, "error", err)
			continue
		}
		refs = append(refs, rows...)
	}
	return refs
}

// ownerClusterRefs reads owner's open cluster findings for database,
// marked as read-only references.
func ownerClusterRefs(ctx context.Context, owner namedPool,
	database string) ([]map[string]any, error) {
	rows, err := owner.pool.Query(ctx, clusterRefsSQL, owner.name, database,
		clusterRefsLimit)
	if err != nil {
		return nil, fmt.Errorf("read the cluster findings of %s: %w", owner.name, err)
	}
	defer rows.Close()
	page, err := scanFindingPage(rows, owner.name, nil)
	if err != nil {
		return nil, fmt.Errorf("read the cluster findings of %s: %w", owner.name, err)
	}
	out := make([]map[string]any, 0, len(page))
	for _, p := range page {
		p.row["cluster_ref"], p.row["read_only"] = true, true
		out = append(out, p.row)
	}
	return out, nil
}
