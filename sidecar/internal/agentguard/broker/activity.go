package broker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
)

// Attribution sources and the reasons it is reported dropped (G1-10).
const (
	SourcePSS   = "pg_stat_statements"
	SourceAudit = "guard_query_audit"

	ReasonDeallocAdvanced = "dealloc_advanced"
	ReasonStatsReset      = "stats_reset"
	ReasonEntriesMissing  = "entries_missing"
	ReasonPSSUnavailable  = "pg_stat_statements_unavailable"
)

// MaxActivityLimit bounds one page.
const MaxActivityLimit = 200

// ActivityRequest selects a principal's activity on one database.
type ActivityRequest struct {
	PrincipalID string
	From, To    time.Time
	Limit       int
	// Cursor continues the audit entries after a previous page.
	Cursor string
}

// Activity is what a principal did on one database: its live sessions, its
// statements as pg_stat_statements attributes them to its roles, and its
// agent_query audit rows. Attribution says whether the statements are
// complete; when they may not be, the audit rows are the record.
type Activity struct {
	Database    string       `json:"database"`
	Roles       []string     `json:"roles"`
	Sessions    []Session    `json:"sessions"`
	Statements  []Statement  `json:"statements"`
	Queries     []AuditEntry `json:"queries"`
	NextCursor  string       `json:"next_cursor,omitempty"`
	Attribution Attribution  `json:"attribution"`
}

// Session is a live backend of one of the principal's roles.
type Session struct {
	PID             int        `json:"pid"`
	Role            string     `json:"role"`
	ApplicationName string     `json:"application_name"`
	State           string     `json:"state"`
	BackendStart    *time.Time `json:"backend_start,omitempty"`
	QueryStart      *time.Time `json:"query_start,omitempty"`
}

// Statement is one pg_stat_statements entry of the principal's roles.
type Statement struct {
	QueryID     string  `json:"query_id"`
	Role        string  `json:"role"`
	Query       string  `json:"query"`
	Calls       int64   `json:"calls"`
	Rows        int64   `json:"rows"`
	TotalExecMS float64 `json:"total_exec_ms"`
}

// AuditEntry is one guard_query_audit row.
type AuditEntry struct {
	ID           int64     `json:"id"`
	At           time.Time `json:"at"`
	Verdict      string    `json:"verdict"`
	Reason       string    `json:"reason,omitempty"`
	Step         string    `json:"step,omitempty"`
	RowCount     *int      `json:"row_count,omitempty"`
	Classes      []string  `json:"classes"`
	EnvelopeHash string    `json:"envelope_hash"`
	TaskID       string    `json:"task_id,omitempty"`
}

// Attribution reports whether the statements are complete.
type Attribution struct {
	Source            string `json:"source"`
	Complete          bool   `json:"complete"`
	Dropped           bool   `json:"dropped"`
	Reason            string `json:"reason,omitempty"`
	Dealloc           int64  `json:"dealloc"`
	AuditedExecutions int    `json:"audited_executions"`
	AttributedCalls   int64  `json:"attributed_calls"`
}

// attributionInput is what the decision compares.
type attributionInput struct {
	Available bool
	Current   pssInfo
	// Earliest is the snapshot on the oldest audited call since the last
	// reset (nil: none).
	Earliest *pssInfo
	Executed int   // successful agent_query executions audited since the reset
	Calls    int64 // their DECLAREs pg_stat_statements still counts
}

// evaluateAttribution reports dropped attribution when pg_stat_statements
// evicted entries (dealloc advanced) or was reset since the oldest audited
// call, or when it counts fewer brokered calls than the audit records.
func evaluateAttribution(in attributionInput) Attribution {
	a := Attribution{Source: SourcePSS, Complete: true, Dealloc: in.Current.Dealloc,
		AuditedExecutions: in.Executed, AttributedCalls: in.Calls}
	drop := func(reason string) Attribution {
		a.Source, a.Complete, a.Dropped, a.Reason = SourceAudit, false, true, reason
		return a
	}
	switch {
	case !in.Available:
		return drop(ReasonPSSUnavailable)
	case in.Earliest != nil && !in.Earliest.Reset.Equal(in.Current.Reset):
		return drop(ReasonStatsReset)
	case in.Earliest != nil && in.Current.Dealloc > in.Earliest.Dealloc:
		return drop(ReasonDeallocAdvanced)
	case int64(in.Executed) > in.Calls:
		return drop(ReasonEntriesMissing)
	}
	return a
}

// ReadActivity reads a principal's activity on t with pg_sage's own pool.
func ReadActivity(ctx context.Context, t Target, req ActivityRequest) (Activity, error) {
	if req.Limit < 1 || req.Limit > MaxActivityLimit || req.PrincipalID == "" {
		return Activity{}, invalidf("limit must be 1-%d and a principal is required",
			MaxActivityLimit)
	}
	after, err := parseCursor(req.Cursor)
	if err != nil {
		return Activity{}, err
	}
	if t.Pool == nil {
		return Activity{}, fmt.Errorf("%w: no pool for %s", ErrUnavailable, t.Name)
	}
	roles := []string{agentguard.BrokerRoleName(req.PrincipalID),
		agentguard.LoginRoleName(req.PrincipalID)}
	a := Activity{Database: t.Name, Roles: roles}
	if a.Sessions, err = readSessions(ctx, t.Pool, roles); err != nil {
		return Activity{}, err
	}
	if a.Queries, a.NextCursor, err = readAudit(ctx, t, req, after); err != nil {
		return Activity{}, err
	}
	in, stmts, err := readAttribution(ctx, t, req, roles)
	if err != nil {
		return Activity{}, err
	}
	a.Statements, a.Attribution = stmts, evaluateAttribution(in)
	if a.Statements == nil {
		a.Statements = []Statement{}
	}
	return a, nil
}

func parseCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || id < 1 {
		return 0, invalidf("cursor is not one this endpoint returned")
	}
	return id, nil
}

func readSessions(ctx context.Context, pool *pgxpool.Pool, roles []string) ([]Session,
	error) {
	rows, err := pool.Query(ctx, `/* pg_sage guard_attribution v1 */
		SELECT pid, usename::text, coalesce(application_name, ''), coalesce(state, ''),
			backend_start, query_start
		FROM pg_catalog.pg_stat_activity
		WHERE usename = ANY($1::text[]) AND datname = pg_catalog.current_database()
		ORDER BY pid`, roles)
	if err != nil {
		return nil, fmt.Errorf("%w: reading agent sessions: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.PID, &s.Role, &s.ApplicationName, &s.State, &s.BackendStart,
			&s.QueryStart); err != nil {
			return nil, fmt.Errorf("%w: reading agent sessions: %v", ErrUnavailable, err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func readAudit(ctx context.Context, t Target, req ActivityRequest, after int64) (
	[]AuditEntry, string, error) {
	rows, err := t.Pool.Query(ctx, `/* pg_sage guard_attribution v1 */
		SELECT id, at, verdict, coalesce(reason, ''), coalesce(step, ''), row_count,
			coalesce(classes, '{}'), envelope_hash, coalesce(task_id, '')
		FROM sage.guard_query_audit
		WHERE principal_id = $1 AND database_id = $2::uuid AND at >= $3 AND at < $4
			AND ($5::bigint = 0 OR id < $5)
		ORDER BY principal_id, at DESC, id DESC LIMIT $6`,
		req.PrincipalID, t.DatabaseID, req.From, req.To, after, req.Limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("%w: reading the query audit: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Verdict, &e.Reason, &e.Step, &e.RowCount,
			&e.Classes, &e.EnvelopeHash, &e.TaskID); err != nil {
			return nil, "", fmt.Errorf("%w: reading the query audit: %v", ErrUnavailable, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("%w: reading the query audit: %v", ErrUnavailable, err)
	}
	next := ""
	if len(out) > req.Limit {
		out = out[:req.Limit]
		next = strconv.FormatInt(out[len(out)-1].ID, 10)
	}
	return out, next, nil
}

// readAttribution reads the principal's pg_stat_statements entries and the
// audit counts the decision compares; without the extension it reports
// unavailable and lists no statements.
func readAttribution(ctx context.Context, t Target, req ActivityRequest, roles []string) (
	attributionInput, []Statement, error) {
	info := readPSSInfo(ctx, t.Pool)
	if info == nil {
		in, err := auditCounts(ctx, t, req, time.Time{})
		in.Available = false
		return in, nil, err
	}
	in, err := auditCounts(ctx, t, req, info.Reset)
	if err != nil {
		return in, nil, err
	}
	in.Available, in.Current = true, *info
	stmts, calls, err := readStatements(ctx, t.Pool, roles)
	if errors.Is(err, errNoPSSView) {
		in.Available = false
		return in, nil, nil
	}
	in.Calls = calls
	return in, stmts, err
}

// auditCounts reads the snapshot on the oldest successful execution in the
// window, and counts successful executions since the last reset.
func auditCounts(ctx context.Context, t Target, req ActivityRequest, since time.Time) (
	attributionInput, error) {
	var in attributionInput
	var dealloc *int64
	var reset *time.Time
	err := t.Pool.QueryRow(ctx, `/* pg_sage guard_attribution v1 */
		SELECT (count(*) FILTER (WHERE at >= $3))::int,
			(array_agg(pss_dealloc ORDER BY at, id) FILTER (WHERE at >= $4 AND at < $5))[1],
			(array_agg(pss_reset ORDER BY at, id) FILTER (WHERE at >= $4 AND at < $5))[1]
		FROM sage.guard_query_audit
		WHERE principal_id = $1 AND database_id = $2::uuid AND at >= least($3, $4)
			AND verdict = 'execute' AND reason IS NULL`,
		req.PrincipalID, t.DatabaseID, since, req.From, req.To).
		Scan(&in.Executed, &dealloc, &reset)
	if err != nil {
		return in, fmt.Errorf("%w: counting audited executions: %v", ErrUnavailable, err)
	}
	if dealloc != nil && reset != nil {
		in.Earliest = &pssInfo{Dealloc: *dealloc, Reset: *reset}
	}
	return in, nil
}

// errNoPSSView: the extension is preloaded but not created in this
// database.
var errNoPSSView = errors.New("pg_stat_statements view not installed")

// readStatements lists the roles' entries in this database and sums the
// calls of the broker's cursor declarations.
func readStatements(ctx context.Context, pool *pgxpool.Pool, roles []string) (
	[]Statement, int64, error) {
	rows, err := pool.Query(ctx, `/* pg_sage guard_attribution v1 */
		SELECT s.queryid::text, r.rolname::text, s.query, s.calls, s.rows,
			s.total_exec_time
		FROM pg_stat_statements s JOIN pg_catalog.pg_roles r ON r.oid = s.userid
		WHERE r.rolname = ANY($1::text[]) AND s.dbid = (SELECT oid FROM
			pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
		ORDER BY s.total_exec_time DESC, s.queryid`, roles)
	if err != nil {
		if pgErr, ok := asPgError(err); ok && pgErr.Code == "42P01" {
			return nil, 0, errNoPSSView
		}
		return nil, 0, fmt.Errorf("%w: reading pg_stat_statements: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	out := []Statement{}
	var calls int64
	for rows.Next() {
		var s Statement
		if err := rows.Scan(&s.QueryID, &s.Role, &s.Query, &s.Calls, &s.Rows,
			&s.TotalExecMS); err != nil {
			return nil, 0, fmt.Errorf("%w: reading pg_stat_statements: %v", ErrUnavailable,
				err)
		}
		if strings.HasPrefix(s.Query, "DECLARE "+cursorName+" ") {
			calls += s.Calls
		}
		out = append(out, s)
	}
	return out, calls, rows.Err()
}
