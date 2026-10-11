package broker

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// deniedNames and deniedPrefixes are the §6.8 S3 additions to the analyze
// guard's deny-list. Most are volatile anyway; the list holds even if a
// catalog marks one stable.
// (Backend signalling is matched by prefix: pg_terminate_*, pg_cancel_*.)
var deniedNames = map[string]bool{"set_config": true, "pg_read_file": true,
	"pg_read_binary_file": true}

var deniedPrefixes = []string{"dblink", "lo_", "pg_ls_", "pg_advisory", "pg_try_advisory",
	"pg_sleep", "pg_terminate_", "pg_cancel_"}

// deniedFunction reports a function agent reads may never call, including
// anything in pg_sage's own schemas.
func deniedFunction(n sqlast.QualifiedName) bool {
	if n.Schema == "sage" || n.Schema == "sage_guard" {
		return true
	}
	if deniedNames[n.Name] {
		return true
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(n.Name, p) {
			return true
		}
	}
	return false
}

// readableCatalog is R0: the pg_catalog relations needed to describe the
// profile's objects. pg_proc (prosrc), pg_authid, role and statistics
// views are not on it.
var readableCatalog = map[string]bool{"pg_class": true, "pg_namespace": true,
	"pg_attribute": true, "pg_attrdef": true, "pg_index": true, "pg_constraint": true,
	"pg_type": true, "pg_description": true, "pg_tables": true, "pg_views": true,
	"pg_indexes": true, "pg_sequences": true, "pg_enum": true, "pg_inherits": true}

func catalogAllowed(schema, name string) bool {
	return schema == "pg_catalog" && readableCatalog[name]
}

// failure is a database error as the agent sees it: the SQLSTATE and a
// message pg_sage wrote. Server text can carry data (a cast error quotes
// the value), so it never reaches the agent.
type failure struct {
	SQLState  string
	Message   string
	Retryable bool
}

func sanitize(pgErr *pgconn.PgError, timeout time.Duration) failure {
	code := pgErr.Code
	f := failure{SQLState: code}
	switch {
	case code == "42501":
		f.Message = "permission denied: the agent's role lacks a privilege this query " +
			"needs (request it with agent_request_capability)"
	case code == "57014":
		f.Message = fmt.Sprintf("the query exceeded the broker's statement timeout of %s",
			timeout)
	case code == "55P03":
		f.Message, f.Retryable = "the query waited too long for a lock", true
	case code == "40001":
		f.Message, f.Retryable = "a serialization failure; retry the query", true
	case code == "40P01":
		f.Message, f.Retryable = "a deadlock was detected; retry the query", true
	case code == "25006":
		f.Message = "the query attempted a write in a read-only transaction"
	case code == "42P01" || code == "42703" || code == "42883" || code == "3F000":
		f.Message = "the query names an object that does not exist or is not visible"
	case strings.HasPrefix(code, "22"):
		f.Message = "a data exception (details withheld: they can contain values)"
	case strings.HasPrefix(code, "42"):
		f.Message = "the query is not valid SQL for this database"
	default:
		f.Message = "the query failed (SQLSTATE " + code + ")"
	}
	return f
}

// asPgError extracts a server error.
func asPgError(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	ok := errors.As(err, &pgErr)
	return pgErr, ok
}

// columnAction is what happens to an output column.
type columnAction int

const (
	actPass columnAction = iota
	actMask
	actDeny
)

func (a columnAction) String() string {
	return [...]string{"pass", "mask", "deny"}[a]
}

// decideColumn applies §6.7/§6.8 S5 to a column of class c in env: secret
// is never returned; pii is masked in branch and dev and refused in stage
// and prod, unless an unmask entry covers it. An unknown env is prod.
func decideColumn(env envbind.Env, c classify.Class, unmasked bool) columnAction {
	switch {
	case c == classify.ClassSecret:
		return actDeny
	case c != classify.ClassPII || unmasked:
		return actPass
	case classify.ShouldMask(env, c, unmasked):
		return actMask
	}
	return actDeny
}

// maskedRefRefusal refuses a statement that references a masked column
// outside the bare output (a filter, a cast, a function argument), or a
// name that is no column of the referenced relations (a whole-row
// reference): either could reveal the masked value through a predicate or
// an error message. known holds every column name of those relations.
func maskedRefRefusal(masked, known map[string]bool, other []string) string {
	if len(masked) == 0 {
		return ""
	}
	var bad []string
	for _, name := range other {
		if masked[name] || !known[name] {
			bad = append(bad, name)
		}
	}
	if len(bad) == 0 {
		return ""
	}
	sort.Strings(bad)
	return "the query uses " + strings.Join(bad, ", ") + " outside its output while a " +
		"masked column is in scope; select masked columns only as plain output"
}

// blocked is a refusal result.
func blocked(reason, detail, fix string) Result {
	return Result{Verdict: VerdictBlocked, ReasonCode: reason, Detail: detail, Fix: fix,
		Columns: []Column{}, Rows: [][]*string{}, Masked: []string{}}
}
