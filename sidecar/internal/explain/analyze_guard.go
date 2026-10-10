package explain

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// EXPLAIN ANALYZE executes the statement. A READ ONLY transaction blocks
// writes to tables but not volatile functions (pg_terminate_backend,
// dblink, set_config, pg_sleep, lo_*, advisory locks ...), so ANALYZE runs
// only when the parse tree and the catalog prove that everything the query
// can invoke is immutable or stable. Anything unknown is refused; the
// caller then returns the plan without ANALYZE and the reason.

// catalogQuerier is the read access the guard needs: the connection that
// holds the explain transaction, so name resolution uses its search_path.
type catalogQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const (
	maxViewDepth    = 4  // nested views followed before refusing
	maxViewsChecked = 16 // views inspected per request
)

// sqlRunningFunctions execute SQL text or read whole relations named only
// by an argument (so the relation guard never sees them), or evaluate a
// type's input function and domain checks by name. Several are STABLE in
// pg_proc, so volatility alone would let them through.
var sqlRunningFunctions = map[string]bool{
	"query_to_xml": true, "query_to_xmlschema": true, "query_to_xml_and_xmlschema": true,
	"cursor_to_xml": true, "cursor_to_xmlschema": true,
	"table_to_xml": true, "table_to_xmlschema": true, "table_to_xml_and_xmlschema": true,
	"schema_to_xml": true, "schema_to_xmlschema": true, "schema_to_xml_and_xmlschema": true,
	"database_to_xml": true, "database_to_xmlschema": true,
	"database_to_xml_and_xmlschema": true, "ts_stat": true,
	"pg_input_is_valid": true, "pg_input_error_info": true,
}

// analyzeRefusal returns why ANALYZE must not run sql, or "" when it is
// proven safe. A catalog failure refuses (fail closed) with its cause.
func (ex *Explainer) analyzeRefusal(
	ctx context.Context, q catalogQuerier, sql string,
) string {
	g := &analyzeGuard{inspect: ex.inspector(), q: q}
	reason, err := g.check(ctx, sql, 0)
	if err != nil {
		ex.logFn("WARN", "explain: ANALYZE safety check failed: %v", err)
		return "could not verify the query against the catalog: " + err.Error()
	}
	return reason
}

func (ex *Explainer) inspector() func(string) (sqlast.ReadQuery, error) {
	if ex.inspect != nil {
		return ex.inspect
	}
	return sqlast.InspectReadQuery
}

type analyzeGuard struct {
	inspect func(string) (sqlast.ReadQuery, error)
	q       catalogQuerier
	views   int
	opts    ReadProofOptions
}

func (g *analyzeGuard) check(ctx context.Context, sql string, depth int) (string, error) {
	shape, err := g.inspect(sql)
	if err != nil {
		return "could not inspect the query: " + err.Error(), nil
	}
	if reason := structuralRefusal(shape); reason != "" {
		return reason, nil
	}
	if reason := g.deniedByName(shape); reason != "" {
		return reason, nil
	}
	steps := []func(context.Context, sqlast.ReadQuery) (string, error){
		g.checkFunctions, g.checkDefiners, g.checkAttributeCalls, g.checkOperators,
		g.checkTypes,
	}
	for _, step := range steps {
		if reason, err := step(ctx, shape); err != nil || reason != "" {
			return reason, err
		}
	}
	return g.checkRelations(ctx, shape, depth)
}

// structuralRefusal covers what needs no catalog: row locks, writes,
// table creation and functions that run SQL text.
func structuralRefusal(q sqlast.ReadQuery) string {
	switch {
	case q.LockingClause:
		return "uses FOR UPDATE/SHARE row locking"
	case q.ModifiesData:
		return "contains a data-modifying statement (INSERT/UPDATE/DELETE/MERGE)"
	case q.SelectInto:
		return "uses SELECT INTO, which creates a table"
	}
	for _, list := range [][]sqlast.QualifiedName{q.Functions, q.AttributeCalls} {
		for _, fn := range list {
			if sqlRunningFunctions[fn.Name] || strings.HasPrefix(fn.Name, "dblink") {
				return fmt.Sprintf("calls %s, which runs SQL or reads relations by name",
					fn.String())
			}
		}
	}
	return ""
}

func (g *analyzeGuard) checkFunctions(ctx context.Context, q sqlast.ReadQuery) (string, error) {
	found, err := resolveFunctions(ctx, g.q, q.Functions)
	if err != nil {
		return "", err
	}
	for _, fn := range q.Functions {
		switch r := found[fn]; {
		case r.matches == 0:
			return "calls unknown function " + fn.String(), nil
		case r.unsafe > 0:
			return "calls volatile function " + fn.String(), nil
		}
	}
	return "", nil
}

// checkAttributeCalls refuses x.name when a volatile function name(x) could
// be what PostgreSQL resolves; with no such function it is a column.
func (g *analyzeGuard) checkAttributeCalls(
	ctx context.Context, q sqlast.ReadQuery,
) (string, error) {
	found, err := resolveFunctions(ctx, g.q, q.AttributeCalls)
	if err != nil {
		return "", err
	}
	for _, fn := range q.AttributeCalls {
		if found[fn].unsafe > 0 {
			return "may call volatile function " + fn.String() +
				" through attribute notation", nil
		}
	}
	return "", nil
}

func (g *analyzeGuard) checkOperators(ctx context.Context, q sqlast.ReadQuery) (string, error) {
	found, err := resolveOperators(ctx, g.q, q.Operators)
	if err != nil {
		return "", err
	}
	for _, op := range q.Operators {
		switch r := found[op]; {
		case r.matches == 0:
			return "uses unknown operator " + op.String(), nil
		case r.unsafe > 0:
			return "uses operator " + op.String() + " backed by a volatile function", nil
		}
	}
	return "", nil
}

func (g *analyzeGuard) checkTypes(ctx context.Context, q sqlast.ReadQuery) (string, error) {
	found, err := resolveTypes(ctx, g.q, q.Types)
	if err != nil {
		return "", err
	}
	for _, typ := range q.Types {
		switch r := found[typ]; {
		case r.matches == 0:
			return "casts to unknown type " + typ.String(), nil
		case r.unsafe > 0:
			return "casts to " + typ.String() +
				", a domain or a type with a volatile input or cast function", nil
		}
	}
	return "", nil
}

// relationKindRefusal judges a resolved non-view relation.
func relationKindRefusal(rel sqlast.QualifiedName, kind string, rls bool) string {
	switch {
	case kind == "f":
		return "reads foreign table " + rel.String() + ", which runs remote or external code"
	case rls && (kind == "r" || kind == "p"):
		return "reads " + rel.String() + ", which has row-level security policies"
	case kind == "r" || kind == "p" || kind == "m" || kind == "t" || kind == "S":
		return ""
	}
	return fmt.Sprintf("reads %s: unsupported relation kind %q", rel.String(), kind)
}

// explainNote tells the caller why a result carries no ANALYZE timing.
func explainNote(planOnly, hasParams bool, refused string) string {
	switch {
	case refused != "":
		return "EXPLAIN without ANALYZE: ANALYZE refused because the query " + refused
	case hasParams:
		return "EXPLAIN without ANALYZE (query has parameters)"
	case planOnly:
		return "EXPLAIN without ANALYZE (plan_only requested)"
	}
	return ""
}

// guardAnalyze runs the analyze guard inside a savepoint, so a failed
// catalog lookup cannot abort the transaction the EXPLAIN still needs.
func (ex *Explainer) guardAnalyze(
	ctx context.Context, conn *pgxpool.Conn, query string,
) string {
	if _, err := conn.Exec(ctx, "SAVEPOINT sage_analyze_guard"); err != nil {
		return "could not open a savepoint for the safety check: " + err.Error()
	}
	refused := ex.analyzeRefusal(ctx, conn, query)
	if _, err := conn.Exec(ctx, "ROLLBACK TO SAVEPOINT sage_analyze_guard"); err != nil {
		ex.logFn("WARN", "explain: release analyze guard savepoint: %v", err)
	}
	return refused
}

// timeout is the statement_timeout and request deadline. timeout_ms <= 0
// means the default: a zero statement_timeout would mean no limit.
func (ex *Explainer) timeout() time.Duration {
	if ex.cfg == nil || ex.cfg.TimeoutMs <= 0 {
		return time.Duration(config.DefaultExplainTimeoutMs) * time.Millisecond
	}
	return time.Duration(ex.cfg.TimeoutMs) * time.Millisecond
}

// statementTimeoutSQL renders the SET LOCAL for d, never below 1ms
// (statement_timeout = 0 disables the limit).
func statementTimeoutSQL(d time.Duration) string {
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", ms)
}
