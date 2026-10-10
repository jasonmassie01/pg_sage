package agentguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// BackendFinding is one agent role seen connected that breaks the §6.6
// rules (G1-01).
type BackendFinding struct {
	Role     string   `json:"role"`
	Sessions int      `json:"sessions"`
	Problems []string `json:"problems"`
}

// agentBackendsSQL lists the agent-named roles with client sessions. Without
// pg_read_all_stats the usename of other roles' sessions stays visible.
const agentBackendsSQL = `/* pg_sage guard_backend_check v1 */
SELECT a.usename::text, count(*)::int FROM pg_catalog.pg_stat_activity a
WHERE a.usename ~ '^sage_agentb?_' AND a.usesysid IS NOT NULL
GROUP BY a.usename ORDER BY a.usename`

// CheckBackends runs G1-01 on q's cluster: every backend whose usename
// matches ^sage_agentb?_ must be a registered agent role of this cluster,
// not superuser or BYPASSRLS (nor CREATEROLE, CREATEDB, REPLICATION),
// without a dangerous membership, and own nothing. It returns the roles
// that fail; none is a pass.
func CheckBackends(ctx context.Context, q Querier, store *Store,
	clusterKey string) ([]BackendFinding, error) {
	rows, err := q.Query(ctx, agentBackendsSQL)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading agent backends: %w", err)
	}
	type seen struct {
		role string
		n    int
	}
	sessions, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seen, error) {
		var s seen
		return s, r.Scan(&s.role, &s.n)
	})
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading agent backends: %w", err)
	}
	var out []BackendFinding
	for _, s := range sessions {
		problems, err := backendProblems(ctx, q, store, clusterKey, s.role)
		if err != nil {
			return nil, err
		}
		if len(problems) > 0 {
			out = append(out, BackendFinding{Role: s.role, Sessions: s.n, Problems: problems})
		}
	}
	return out, nil
}

func backendProblems(ctx context.Context, q Querier, store *Store, clusterKey,
	role string) ([]string, error) {
	var problems []string
	reg, err := store.RoleByName(ctx, clusterKey, role)
	switch {
	case errors.Is(err, ErrNotFound):
		problems = append(problems, "is not a registered agent role")
	case err != nil:
		return nil, err
	case reg.Status != RoleStatusActive:
		problems = append(problems, "is registered as "+reg.Status+" but connected")
	}
	st, err := ReadRoleState(ctx, q, role)
	if errors.Is(err, ErrNotFound) {
		return problems, nil // dropped since the session list was read
	}
	if err != nil {
		return nil, err
	}
	return append(problems, st.AgentViolations()...), nil
}
