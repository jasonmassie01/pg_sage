package collector

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Workload sessions (market-08): who is using this database right now,
// for workload budgets. Four bounded reads per cycle, whether or not any
// budget is configured (plus the connection ages of evid-02b and
// review-02e):
//
//   - this database's client sessions grouped by (role, application,
//     state), at most 200 groups (one more row detects truncation);
//   - its 50 longest-running sessions past one second (active by query
//     age, idle in transaction by state age), with the sqlcommenter tags
//     of the active ones;
//   - per-role statement time from pg_stat_statements.userid, at most
//     200 roles (only where the extension resolves);
//   - the cluster's client sessions per role, at most 200 roles: a role's
//     connection limit is cluster-wide.
//
// pg_sage's own sessions are left out of the first two (notSelf).

const (
	workloadGroupCap = 200
	workloadLongCap  = 50
	// queryTextCap bounds the query text read per long session; tags are
	// parsed from it (track_activity_query_size is 1 kB by default).
	queryTextCap = 4096
)

// WorkloadGroup is the client sessions of one (role, application, state).
type WorkloadGroup struct {
	Role               string  `json:"role"`
	Application        string  `json:"application"`
	State              string  `json:"state"`
	Sessions           int     `json:"sessions"`
	MaxXactAgeSeconds  float64 `json:"max_xact_age_seconds"`
	MaxStateAgeSeconds float64 `json:"max_state_age_seconds"`
	// MaxQueryAgeSeconds is the longest-running active query's age; 0 for
	// a state other than active.
	MaxQueryAgeSeconds float64 `json:"max_query_age_seconds"`
}

// WorkloadLongSession is one session past one second in its state. PID
// and QueryStart identify one statement across cycles.
type WorkloadLongSession struct {
	PID         int               `json:"pid"`
	Role        string            `json:"role"`
	Application string            `json:"application"`
	State       string            `json:"state"`
	QueryStart  time.Time         `json:"query_start"`
	AgeSeconds  float64           `json:"age_seconds"`
	Tags        map[string]string `json:"tags"`
}

// WorkloadRoleStats is one role's statement time in this database
// (pg_stat_statements, cumulative since its reset) and its client
// sessions cluster-wide. P99EstimateMs is the largest per-statement
// mean + 2.326 standard deviations, capped at the statement's maximum:
// pg_stat_statements keeps no percentiles, and the largest is the
// conservative (longer) estimate.
type WorkloadRoleStats struct {
	Role            string  `json:"role"`
	Calls           int64   `json:"calls"`
	MaxExecMs       float64 `json:"max_exec_ms"`
	P99EstimateMs   float64 `json:"p99_estimate_ms"`
	ClusterSessions int     `json:"cluster_sessions"`
}

// WorkloadSessions is the workload_sessions category. Its slices are
// non-nil after a successful read.
type WorkloadSessions struct {
	Groups    []WorkloadGroup       `json:"groups"`
	Truncated bool                  `json:"truncated"`
	Long      []WorkloadLongSession `json:"long"`
	Roles     []WorkloadRoleStats   `json:"roles"`
	// Ages is the sessions by state and connection-age bucket (evid-02b);
	// nil (absent) on snapshots written before it.
	Ages []WorkloadAgeBucket `json:"ages,omitempty"`
	// AppAges is the sessions by application and connection minute
	// (review-02e), at most WorkloadAppAgeCap rows (AppAgesTruncated);
	// nil (absent) on snapshots written before it.
	AppAges          []WorkloadAppAge `json:"app_ages,omitempty"`
	AppAgesTruncated bool             `json:"app_ages_truncated,omitempty"`
}

var workloadSessionFilter = `
 WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
   AND datname = current_database() AND ` + notSelf

var workloadGroupsSQL = sageTag + `
SELECT COALESCE(usename, ''), COALESCE(application_name, ''),
       COALESCE(state, 'unknown'), count(*)::int,
       COALESCE(max(EXTRACT(EPOCH FROM (now() - xact_start))), 0)::float8,
       COALESCE(max(EXTRACT(EPOCH FROM (now() - state_change))), 0)::float8,
       COALESCE(max(EXTRACT(EPOCH FROM (now() - query_start)))
         FILTER (WHERE state = 'active'), 0)::float8
  FROM pg_stat_activity` + workloadSessionFilter + `
 GROUP BY 1, 2, 3
 ORDER BY 4 DESC, 1, 2, 3
 LIMIT 201`

var workloadLongSQL = sageTag + `
SELECT pid, COALESCE(usename, ''), COALESCE(application_name, ''), state,
       query_start, EXTRACT(EPOCH FROM (now() - CASE WHEN state = 'active'
         THEN query_start ELSE state_change END))::float8 AS age,
       CASE WHEN state = 'active' THEN left(query, ` + fmt.Sprint(queryTextCap) + `)
         ELSE '' END
  FROM pg_stat_activity` + workloadSessionFilter + `
   AND ((state = 'active' AND query_start < now() - interval '1 second')
     OR (state LIKE 'idle in transaction%' AND state_change < now() - interval '1 second'))
 ORDER BY age DESC, pid
 LIMIT ` + fmt.Sprint(workloadLongCap)

// workloadRolesSQL reads per-role statement time from view, the qualified
// pg_stat_statements view.
func workloadRolesSQL(view string) string {
	return sageTag + `
SELECT r.rolname, sum(s.calls)::bigint, max(s.max_exec_time)::float8,
       max(LEAST(s.max_exec_time, s.mean_exec_time + 2.326 * s.stddev_exec_time))::float8
  FROM ` + view + ` s
  JOIN pg_roles r ON r.oid = s.userid
 WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
   AND s.calls > 0
 GROUP BY r.rolname
 ORDER BY 2 DESC, 1
 LIMIT 200`
}

var workloadClusterSessionsSQL = sageTag + `
SELECT COALESCE(usename, ''), count(*)::int
  FROM pg_stat_activity
 WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()
 GROUP BY 1
 ORDER BY 2 DESC, 1
 LIMIT 200`

// collectWorkloadSessions reads the workload_sessions category.
func (c *Collector) collectWorkloadSessions(ctx context.Context) (*WorkloadSessions, error) {
	groups, err := c.workloadGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("workload session groups: %w", err)
	}
	ws := &WorkloadSessions{}
	ws.Groups, ws.Truncated = capWorkloadGroups(groups)
	if ws.Long, err = c.workloadLong(ctx); err != nil {
		return nil, fmt.Errorf("long-running workload sessions: %w", err)
	}
	if ws.Roles, err = c.workloadRoles(ctx); err != nil {
		return nil, fmt.Errorf("workload role statistics: %w", err)
	}
	if ws.Ages, err = c.workloadAges(ctx); err != nil {
		return nil, fmt.Errorf("workload connection ages: %w", err)
	}
	if ws.AppAges, ws.AppAgesTruncated, err = c.workloadAppAges(ctx); err != nil {
		return nil, fmt.Errorf("workload connection ages by application: %w", err)
	}
	return ws, nil
}

// capWorkloadGroups keeps workloadGroupCap groups; more marks truncation.
func capWorkloadGroups(groups []WorkloadGroup) ([]WorkloadGroup, bool) {
	if len(groups) > workloadGroupCap {
		return groups[:workloadGroupCap], true
	}
	return groups, false
}

func (c *Collector) workloadGroups(ctx context.Context) ([]WorkloadGroup, error) {
	rows, err := c.catalogQuery(ctx, workloadGroupsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkloadGroup{}
	for rows.Next() {
		var g WorkloadGroup
		if err := rows.Scan(&g.Role, &g.Application, &g.State, &g.Sessions,
			&g.MaxXactAgeSeconds, &g.MaxStateAgeSeconds, &g.MaxQueryAgeSeconds); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// longSessionRow is one scanned long-session row.
type longSessionRow struct {
	pid              int
	role, app, state string
	queryStart       time.Time
	ageSeconds       float64
	query            string
}

// buildLongSession parses tags from an active query only: an idle in
// transaction session's last statement is not what it is doing now.
func buildLongSession(r longSessionRow) WorkloadLongSession {
	tags := map[string]string{}
	if r.state == "active" {
		tags = sqlast.ParseSQLCommenter(r.query)
	}
	return WorkloadLongSession{PID: r.pid, Role: r.role, Application: r.app,
		State: r.state, QueryStart: r.queryStart, AgeSeconds: r.ageSeconds, Tags: tags}
}

func (c *Collector) workloadLong(ctx context.Context) ([]WorkloadLongSession, error) {
	rows, err := c.catalogQuery(ctx, workloadLongSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkloadLongSession{}
	for rows.Next() {
		var r longSessionRow
		var start *time.Time
		if err := rows.Scan(&r.pid, &r.role, &r.app, &r.state, &start, &r.ageSeconds,
			&r.query); err != nil {
			return nil, err
		}
		if start != nil {
			r.queryStart = *start
		}
		out = append(out, buildLongSession(r))
	}
	return out, rows.Err()
}

// workloadRoles merges the statement time (where pg_stat_statements
// resolves) with the cluster session counts.
func (c *Collector) workloadRoles(ctx context.Context) ([]WorkloadRoleStats, error) {
	byRole := map[string]*WorkloadRoleStats{}
	out := []WorkloadRoleStats{}
	if c.Capabilities().StatStatementsView {
		stats, err := c.workloadRoleTime(ctx)
		if err != nil {
			return nil, err
		}
		out = stats
	}
	for i := range out {
		byRole[out[i].Role] = &out[i]
	}
	counts, err := c.workloadClusterSessions(ctx)
	if err != nil {
		return nil, err
	}
	for role, n := range counts {
		if r, ok := byRole[role]; ok {
			r.ClusterSessions = n
			continue
		}
		out = append(out, WorkloadRoleStats{Role: role, ClusterSessions: n})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Role < out[b].Role })
	return out, nil
}

func (c *Collector) workloadRoleTime(ctx context.Context) ([]WorkloadRoleStats, error) {
	names, err := c.pgss()
	if err != nil {
		return nil, err
	}
	rows, err := c.catalogQuery(ctx, workloadRolesSQL(names.view))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkloadRoleStats{}
	for rows.Next() {
		var r WorkloadRoleStats
		if err := rows.Scan(&r.Role, &r.Calls, &r.MaxExecMs, &r.P99EstimateMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (c *Collector) workloadClusterSessions(ctx context.Context) (map[string]int, error) {
	rows, err := c.catalogQuery(ctx, workloadClusterSessionsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var role string
		var n int
		if err := rows.Scan(&role, &n); err != nil {
			return nil, err
		}
		out[role] = n
	}
	return out, rows.Err()
}
