package explain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
	"github.com/pg-sage/sidecar/internal/planfresh"
)

// ErrUnknownQueryID: no pg_stat_statements entry of the current database
// has the id. Errors carrying it also carry ErrExplainInvalidRequest.
var ErrUnknownQueryID = errors.New("unknown query_id")

// ErrStatementsUnavailable: pg_stat_statements cannot be read here, so a
// query_id cannot be resolved. Errors carrying it also carry
// ErrExplainInvalidRequest: the caller can send the query text instead.
var ErrStatementsUnavailable = errors.New("pg_stat_statements unavailable")

// QueryID is a pg_stat_statements id. JSON accepts an integer or a decimal
// string, both read exactly (a browser rounds integers past 2^53).
type QueryID int64

// UnmarshalJSON reads an integer or a decimal string in canonical form.
func (q *QueryID) UnmarshalJSON(raw []byte) error {
	text := string(bytes.TrimSpace(raw))
	if text == "null" {
		*q = 0
		return nil
	}
	if len(text) > 0 && text[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("%w: query_id %s: %v", ErrExplainInvalidRequest, text, err)
		}
		text = s
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != text {
		return fmt.Errorf("%w: query_id %q is not an exact 64-bit integer",
			ErrExplainInvalidRequest, text)
	}
	*q = QueryID(n)
	return nil
}

// statementTextSQL reads one statement's text in the current database,
// summed over users and top-level/nested entries (NULL when absent).
// rel is the qualified pg_stat_statements view.
func statementTextSQL(rel string) string {
	return `/* pg_sage */ SELECT min(query)
FROM ` + rel + `
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid = $1`
}

// ResolveQueryID returns the pg_stat_statements text of id in the database
// q reads. The REST explain endpoint and the MCP tools share it. An id
// absent from this database (or present only in another) is
// ErrUnknownQueryID; a missing pg_stat_statements is
// ErrStatementsUnavailable; both are invalid requests.
func ResolveQueryID(ctx context.Context, q catalogread.Querier, id int64) (string, error) {
	var text *string
	rel, err := extschema.StatementsRel(ctx, q, "pg_stat_statements")
	if err != nil {
		return "", statementsError(err)
	}
	if err := q.QueryRow(ctx, statementTextSQL(rel), id).Scan(&text); err != nil {
		return "", statementsError(err)
	}
	if text == nil {
		return "", fmt.Errorf("%w: %w: no pg_stat_statements entry %d in this database",
			ErrExplainInvalidRequest, ErrUnknownQueryID, id)
	}
	return *text, nil
}

// statementsError marks a missing or unloaded pg_stat_statements.
func statementsError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42P01", "42883", "55000", "3F000":
			return fmt.Errorf("%w: %w: query_id needs pg_stat_statements, which is not "+
				"usable here (%s); send the query text instead",
				ErrExplainInvalidRequest, ErrStatementsUnavailable, pgErr.Message)
		}
	}
	return fmt.Errorf("read pg_stat_statements: %w", err)
}

// WithPlanMaxAge sets tuner.plan_max_age_minutes, the age past which a
// captured plan is not used (tuning-08); 0 or less means the default.
func (ex *Explainer) WithPlanMaxAge(minutes int) *Explainer {
	ex.planMaxAge = planfresh.MaxAge(minutes)
	return ex
}

func (ex *Explainer) clock() time.Time {
	if ex.now != nil {
		return ex.now()
	}
	return time.Now()
}

// readTimeouts bound the lookups query_id makes: the explain timeout and
// the shipped lock timeout.
func (ex *Explainer) readTimeouts() catalogread.Timeouts {
	return catalogread.Timeouts{Statement: ex.timeout(), Lock: catalogread.Default().Lock}
}

// resolve returns the text of req.QueryID and, when one is fresh
// (planfresh) and executed, its captured plan. Explicit params ask for a
// plan of those values, so no capture is used. A capture that cannot be
// read is logged and the text is explained afresh.
func (ex *Explainer) resolve(ctx context.Context, req ExplainRequest,
) (string, *planfresh.Plan, error) {
	if ex.pool == nil {
		return "", nil, errors.New("query_id lookup needs a database pool")
	}
	db := catalogread.New(ex.pool, ex.readTimeouts())
	id := int64(req.QueryID)
	text, err := ResolveQueryID(ctx, db, id)
	if err != nil || len(req.Params) > 0 {
		return text, nil, err
	}
	r := planfresh.Reader{DB: db, MaxAge: ex.planMaxAge, Now: ex.now}
	p, err := r.Read(ctx, id, text)
	if err != nil {
		ex.logFn("WARN", "explain: captured plan of queryid %d not read: %v", id, err)
		return text, nil, nil
	}
	if p.Source != planfresh.SourceCache || !p.HasActuals {
		return text, nil, nil
	}
	return text, &p, nil
}

// capturedResult explains a captured executed plan: nodes, the rules
// floor and the LLM, labelled with its source and age. Nothing runs on the
// monitored database except the catalog read of the rules, and the result
// is not cached (the capture is already stored).
func (ex *Explainer) capturedResult(ctx context.Context, body string, p planfresh.Plan,
) *ExplainResult {
	result := buildResult(body, json.RawMessage(p.JSON), true)
	age := max(int64(ex.clock().Sub(p.CapturedAt)/time.Second), 0)
	result.PlanSource, result.PlanCapture, result.PlanAgeS = planfresh.SourceCache,
		p.Capture, &age
	result.Note = fmt.Sprintf("plan captured by %s %ds ago; the statement was not run again",
		p.Capture, age)
	ex.applyRules(ctx, result, true)
	ex.enhanceWithLLM(ctx, result)
	return result
}
