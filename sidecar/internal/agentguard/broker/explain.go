package broker

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Agent explain_query (security-02): plan-only EXPLAIN of one SELECT as
// the agent's own broker role. It takes agent_query's path (screening,
// parsing, the gate, the catalog proof, column classes, the read-only
// transaction with the broker's timeouts, the audit row) and returns the
// plan instead of rows. EXPLAIN checks the role's privileges on every
// relation, so a plan never describes what the role could not read.

// explainTool is the MCP tool the gate decides for.
const explainTool = "explain_query"

// explainStatementName is the prepared statement a parameterized explain
// plans through; it is deallocated after the transaction ends.
const explainStatementName = "sage_agent_x"

// Explain plans one agent read. Refusals and database errors are results,
// as for Query; an error is invalid arguments, a database outside the
// token, an unknown database, or an unavailable dependency.
func (b *Broker) Explain(ctx context.Context, req ExplainRequest) (ExplainResult, error) {
	params, err := validateExplain(req)
	if err != nil {
		return ExplainResult{}, err
	}
	c, refused, err := b.open(ctx, req.Database, req.SQL, explainTool)
	if err != nil {
		return ExplainResult{}, err
	}
	if c == nil {
		return explainResultOf(refused, nil), nil
	}
	c.req, c.params, c.explain = Request{Database: req.Database, SQL: req.SQL}, params, true
	res, err := c.run(ctx)
	res, err = c.finish(ctx, res, err)
	if err != nil {
		return ExplainResult{}, err
	}
	return explainResultOf(res, c.planJSON), nil
}

// validateExplain checks an explain's arguments and renders its
// parameters: a missing or empty value is NULL (nil).
func validateExplain(req ExplainRequest) ([][]byte, error) {
	switch {
	case strings.TrimSpace(req.Database) == "":
		return nil, invalidf("database is required")
	case strings.TrimSpace(req.SQL) == "":
		return nil, invalidf("sql is required")
	case len(req.SQL) > MaxSQLBytes:
		return nil, invalidf("sql is longer than %d bytes", MaxSQLBytes)
	case len(req.Params) > MaxParams:
		return nil, invalidf("at most %d params", MaxParams)
	}
	params := make([][]byte, len(req.Params))
	for i, p := range req.Params {
		switch {
		case len(p) > maxParamLen:
			return nil, invalidf("param %d is longer than %d bytes", i+1, maxParamLen)
		case strings.ContainsRune(p, 0):
			return nil, invalidf("param %d contains a NUL byte", i+1)
		case p != "":
			params[i] = []byte(p)
		}
	}
	return params, nil
}

// explainResultOf is a call's result as explain_query reports it.
func explainResultOf(r Result, plan []byte) ExplainResult {
	return ExplainResult{Verdict: r.Verdict, Status: r.Status, ReasonCode: r.ReasonCode,
		Detail: r.Detail, Fix: r.Fix, PlanJSON: plan, EnvelopeHash: r.EnvelopeHash,
		SQLState: r.SQLState, Message: r.Message, Retryable: r.Retryable,
		RetryAfterSeconds: r.RetryAfterSeconds}
}

// paramsFit reports whether the given parameters fit the statement's n:
// agent_query binds every one; an explain may leave some unbound.
func (c *call) paramsFit(n int) bool {
	if c.explain {
		return len(c.params) <= n
	}
	return len(c.params) == n
}

// envelopeStatement is what the envelope hash binds: the canonical
// statement, marked as an EXPLAIN for explain_query, so an explain and a
// query of one statement are told apart in the audit.
func (c *call) envelopeStatement(read sqlast.BrokeredRead) string {
	if c.explain {
		return explainEnvelopeStatement(read.Canonical)
	}
	return read.Canonical
}

func explainEnvelopeStatement(canonical string) string {
	return "EXPLAIN (FORMAT JSON) " + canonical
}

// explainPlan runs plan-only EXPLAIN in the open read-only transaction.
// A statement with parameters is prepared and explained through EXECUTE,
// with the generic plan when a value is missing (a NULL would fold the
// custom plan to a constant).
func (c *call) explainPlan(ctx context.Context, pg *pgconn.PgConn,
	read sqlast.BrokeredRead) (Result, error) {
	stmt := explainEnvelopeStatement(read.Canonical)
	if read.Params > 0 {
		var err error
		if stmt, err = c.prepareExplain(ctx, pg, read); err != nil {
			return c.dbFailure(err)
		}
	}
	rr := pg.ExecParams(ctx, stmt, nil, nil, nil, nil)
	var plan []byte
	for rr.NextRow() {
		for _, v := range rr.Values() {
			plan = append(plan, v...)
		}
	}
	if _, err := rr.Close(); err != nil {
		return c.dbFailure(err)
	}
	if len(plan) == 0 {
		return Result{}, fmt.Errorf("%w: EXPLAIN returned no plan", ErrUnavailable)
	}
	c.planJSON = plan
	return Result{Verdict: VerdictExecute, Status: StatusOK, Columns: []Column{},
		Rows: [][]*string{}, Masked: []string{}}, nil
}

// prepareExplain prepares the statement and returns the EXPLAIN EXECUTE
// that plans it. Values are quoted literals; standard_conforming_strings
// is set here so a role default cannot turn a backslash into an escape.
func (c *call) prepareExplain(ctx context.Context, pg *pgconn.PgConn,
	read sqlast.BrokeredRead) (string, error) {
	args, generic := executeArguments(c.params, read.Params)
	stmts := []string{"SET LOCAL standard_conforming_strings = on"}
	if generic {
		stmts = append(stmts, "SET LOCAL plan_cache_mode = force_generic_plan")
	}
	for _, s := range stmts {
		if _, err := pg.ExecParams(ctx, s, nil, nil, nil, nil).Close(); err != nil {
			return "", err
		}
	}
	prepare := "PREPARE " + explainStatementName + " AS " + read.Canonical
	if _, err := pg.ExecParams(ctx, prepare, nil, nil, nil, nil).Close(); err != nil {
		return "", err
	}
	c.prepared = true
	return fmt.Sprintf("EXPLAIN (FORMAT JSON) EXECUTE %s(%s)", explainStatementName, args),
		nil
}

// executeArguments renders EXECUTE's n arguments: given values as quoted
// literals, NULL otherwise; generic reports a NULL among them.
func executeArguments(params [][]byte, n int) (string, bool) {
	values := make([]string, n)
	generic := false
	for i := range values {
		if i < len(params) && params[i] != nil {
			values[i] = "'" + strings.ReplaceAll(string(params[i]), "'", "''") + "'"
			continue
		}
		values[i] = "NULL"
		generic = true
	}
	return strings.Join(values, ", "), generic
}
