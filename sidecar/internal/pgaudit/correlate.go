package pgaudit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// actionWindow is how far a pgaudit record's time may be from its action's
// executed_at (the action row is written after the statement runs).
const actionWindow = 10 * time.Minute

// Stats counts what one ingest did.
type Stats struct {
	Stored   int // correlated and kept
	Dropped  int // pgaudit records that belong to no principal or action
	NotAudit int // log entries that are not pgaudit records
}

// Correlator keeps the correlated pgaudit records of one database in its
// sage.guard_pgaudit_events (a hash-chained table).
type Correlator struct {
	pool     *pgxpool.Pool
	database string
}

// NewCorrelator returns a correlator writing to pool for database.
func NewCorrelator(pool *pgxpool.Pool, database string) *Correlator {
	return &Correlator{pool: pool, database: database}
}

// event is one row of sage.guard_pgaudit_events.
type event struct {
	DatabaseName   string    `json:"database_name"`
	LoggedAt       time.Time `json:"logged_at"`
	SessionID      string    `json:"session_id"`
	PID            int       `json:"pid"`
	DBUser         string    `json:"db_user"`
	Application    string    `json:"application"`
	PrincipalID    *string   `json:"principal_id"`
	ActionID       *int64    `json:"action_id"`
	AuditType      string    `json:"audit_type"`
	StatementID    int64     `json:"statement_id"`
	SubstatementID int64     `json:"substatement_id"`
	Class          string    `json:"class"`
	Command        string    `json:"command"`
	ObjectType     string    `json:"object_type"`
	ObjectName     string    `json:"object_name"`
	Statement      string    `json:"statement"`
	CorrelatedBy   string    `json:"correlated_by"`
}

// Ingest correlates entries and stores the records that belong to an agent
// principal, or to one of pg_sage's own actions. pg_sage's other
// statements (its collectors' reads) are dropped: they are pg_sage's own
// monitoring, already visible in pg_stat_statements.
func (c *Correlator) Ingest(ctx context.Context, entries []logwatch.LogEntry) (Stats, error) {
	var st Stats
	var rows []event
	for _, e := range entries {
		rec, ok := Parse(e.Message)
		if !ok {
			st.NotAudit++
			continue
		}
		a, ok := Attribute(e)
		if ok && a.By == ByPGSage {
			id, err := c.actionFor(ctx, rec.Statement, e.Timestamp)
			if err != nil {
				return st, err
			}
			a.By, a.ActionID, ok = ByStatement, id, id > 0
		}
		if !ok {
			st.Dropped++
			continue
		}
		rows = append(rows, c.row(e, rec, a))
	}
	if len(rows) == 0 {
		return st, nil
	}
	if err := c.store(ctx, rows); err != nil {
		return st, err
	}
	st.Stored = len(rows)
	return st, nil
}

func (c *Correlator) row(e logwatch.LogEntry, rec Record, a Attribution) event {
	ev := event{DatabaseName: c.database, LoggedAt: e.Timestamp, SessionID: e.SessionID,
		PID: e.PID, DBUser: e.User, Application: e.Application, AuditType: rec.AuditType,
		StatementID: rec.StatementID, SubstatementID: rec.SubstatementID,
		Class: rec.Class, Command: rec.Command, ObjectType: rec.ObjectType,
		ObjectName: rec.ObjectName, Statement: rec.Statement, CorrelatedBy: a.By}
	if a.Principal != "" {
		p := a.Principal
		ev.PrincipalID = &p
	}
	if a.ActionID > 0 {
		id := a.ActionID
		ev.ActionID = &id
	}
	return ev
}

// store inserts the batch in one statement (each row is chained at commit).
func (c *Correlator) store(ctx context.Context, rows []event) error {
	body, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("pgaudit: encode records: %w", err)
	}
	_, err = c.pool.Exec(ctx, `/* pg_sage pgaudit v1 */ INSERT INTO sage.guard_pgaudit_events
		(database_name, logged_at, session_id, pid, db_user, application, principal_id,
		 action_id, audit_type, statement_id, substatement_id, class, command,
		 object_type, object_name, statement, correlated_by)
		SELECT database_name, logged_at, session_id, pid, db_user, application,
		 principal_id, action_id, audit_type, statement_id, substatement_id, class,
		 command, object_type, object_name, statement, correlated_by
		FROM jsonb_to_recordset($1::jsonb) AS r(database_name text, logged_at timestamptz,
		 session_id text, pid integer, db_user text, application text, principal_id text,
		 action_id bigint, audit_type text, statement_id bigint, substatement_id bigint,
		 class text, command text, object_type text, object_name text, statement text,
		 correlated_by text)`, string(body))
	if err != nil {
		return fmt.Errorf("pgaudit: store %d records: %w", len(rows), err)
	}
	return nil
}

// actionFor finds the action whose SQL a pg_sage statement executed, near
// its time, through the (md5(sql_executed), executed_at) index. pg_sage tags
// its statements with a leading comment, which the match ignores.
func (c *Correlator) actionFor(ctx context.Context, statement string, at time.Time) (
	int64, error) {
	candidates := []string{statement}
	if bare := stripLeadingComments(statement); bare != statement {
		candidates = append(candidates, bare)
	}
	for _, sql := range candidates {
		var id int64
		err := c.pool.QueryRow(ctx, `/* pg_sage pgaudit v1 */ SELECT id FROM sage.action_log
			WHERE md5(sql_executed) = md5($1) AND executed_at >= $2 AND executed_at <= $3
			ORDER BY abs(extract(epoch FROM executed_at - $4::timestamptz)) LIMIT 1`,
			sql, at.Add(-actionWindow), at.Add(actionWindow), at).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("pgaudit: match action: %w", err)
		}
	}
	return 0, nil
}

// stripLeadingComments removes leading /* */ comments, white space and a
// trailing semicolon.
func stripLeadingComments(sql string) string {
	s := strings.TrimSpace(sql)
	for strings.HasPrefix(s, "/*") {
		end := strings.Index(s, "*/")
		if end < 0 {
			break
		}
		s = strings.TrimSpace(s[end+2:])
	}
	return strings.TrimSpace(strings.TrimSuffix(s, ";"))
}

// Installed reports whether pgaudit is preloaded on the server (it logs
// nothing otherwise).
func Installed(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (bool, error) {
	var on bool
	err := q.QueryRow(ctx, `/* pg_sage pgaudit v1 */ SELECT 'pgaudit' = ANY(
		string_to_array(replace(current_setting('shared_preload_libraries'), ' ', ''), ','))`).
		Scan(&on)
	if err != nil {
		return false, fmt.Errorf("pgaudit: read shared_preload_libraries: %w", err)
	}
	return on, nil
}
