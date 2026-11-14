package sre

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator's safe EXPLAIN (roadmap 2.1). The model names a
// statement by its pg_stat_statements queryid. The explainer holds the
// database's probe slot (the runner's WithSlot), takes a pool connection
// within its own acquire budget, confirms the queryid with
// pg_stat_statements(showtext => false) in a read-only transaction and
// takes the text from the collector's in-memory snapshot (StatementTexts),
// never from the whole query-text file. It explains plan-only (never
// ANALYZE, so the statement never runs), only when it is one read
// statement that neither writes nor locks rows, under the probe timeouts.
// It is prepared over the extended protocol (one statement only) and
// explained with plan_cache_mode force_generic_plan, so a normalized
// statement's $n parameters are planned generically on every supported
// version, never as constants. The evidence carries plan node shapes
// (node type, relation, index, join type, costs, row estimates), never
// the text or its predicates.

// ExplainProbeID is the evidence id of a plan-only EXPLAIN.
const ExplainProbeID probes.ID = "explain_statement"

// Explain refusals.
const (
	ReasonNotReadStatement = "not_a_read_statement"
	ReasonUnknownStatement = "unknown_statement"
	// ReasonTextNotCached is a statement whose text the collector's latest
	// snapshot does not hold (or no text source is wired).
	ReasonTextNotCached = "text_not_cached"
	// ReasonMissingSelect is a role without SELECT on a relation the
	// statement reads (pg_monitor alone cannot plan it).
	ReasonMissingSelect = "missing_select"
)

// maxPlanNodes bounds the plan nodes one EXPLAIN keeps.
const maxPlanNodes = 40

// Explainer explains one statement, plan-only.
type Explainer interface {
	ExplainStatement(ctx context.Context, queryID int64) probes.Result
}

// StatementTexts returns a statement's text by queryid from memory (the
// collector's latest snapshot); false when it is not cached.
type StatementTexts func(queryID int64) (string, bool)

// StatementExplainer explains statements of one monitored database.
type StatementExplainer struct {
	runner *probes.Runner
	texts  StatementTexts
}

var _ Explainer = (*StatementExplainer)(nil)

// NewStatementExplainer explains statements of runner's database inside
// its probe slots, with texts from texts (nil: every statement is
// text_not_cached).
func NewStatementExplainer(runner *probes.Runner, texts StatementTexts) *StatementExplainer {
	return &StatementExplainer{runner: runner, texts: texts}
}

var paramPattern = regexp.MustCompile(`\$(\d+)`)

// placeholders is the highest $n of a statement (0 without any).
func placeholders(body string) int {
	n := 0
	for _, m := range paramPattern.FindAllStringSubmatch(body, -1) {
		if k, err := strconv.Atoi(m[1]); err == nil && k <= 65535 {
			n = max(n, k)
		}
	}
	return n
}

// ExplainStatement explains one statement by queryid. It never panics
// and never returns an untyped failure.
func (x *StatementExplainer) ExplainStatement(ctx context.Context,
	queryID int64) probes.Result {
	if denied := probes.CheckReadPolicy(ctx, nil, ExplainProbeID,
		probes.Args{QueryID: queryID}); denied != nil {
		return *denied
	}
	res := probes.Result{ProbeID: ExplainProbeID, Version: "v1", ObservedAt: time.Now()}
	var pool *pgxpool.Pool
	if x != nil {
		pool = x.runner.Pool()
	}
	switch {
	case pool == nil:
		return failedRead(res, probes.StatusError, "not_configured", nil)
	case queryID == 0:
		return failedRead(res, probes.StatusError, "invalid_args", nil)
	}
	if p := probes.ContextReadPolicy(ctx); p != nil &&
		(p.Database != pool.Config().ConnConfig.Database ||
			p.Binding != probes.PoolBinding(pool)) {
		return probes.PolicyDenied(ExplainProbeID,
			"pool database does not match trusted read policy")
	}
	start := time.Now()
	err := x.runner.WithSlot(ctx, func(ctx context.Context) error {
		res = x.explainOn(ctx, pool, queryID, res)
		return nil
	})
	switch {
	case errors.Is(err, probes.ErrConcurrencyLimit):
		res = failedRead(res, probes.StatusError, "concurrency_limit", err)
		res.Phase = probes.PhaseQueue
	case err != nil:
		res = failedRead(res, probes.StatusError, "not_configured", err)
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res
}

// explainOn takes a connection within the runner's acquire budget, then
// explains in a read-only transaction within the statement budget.
func (x *StatementExplainer) explainOn(ctx context.Context, pool *pgxpool.Pool,
	queryID int64, res probes.Result) probes.Result {
	actx, cancelAcquire := context.WithTimeout(ctx, x.runner.AcquireWait())
	conn, err := pool.Acquire(actx)
	cancelAcquire()
	if err != nil {
		res = readFailure(res, err)
		res.Phase = probes.PhasePoolAcquire
		return res
	}
	defer conn.Release()
	ctx, cancel := context.WithTimeout(ctx, probes.MaxStatementTimeout+time.Second)
	defer cancel()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return readFailure(res, err)
	}
	name := statementName()
	defer func() {
		// A prepared statement survives the rollback; DEALLOCATE fails in
		// an aborted transaction, so it is released after it.
		_ = tx.Rollback(context.Background())
		dctx, done := context.WithTimeout(context.Background(), probes.MaxStatementTimeout)
		defer done()
		_, _ = conn.Exec(dctx, "DEALLOCATE "+name)
	}()
	return x.explainIn(ctx, tx, name, queryID, res)
}

func statementName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sage_inv_explain_" + hex.EncodeToString(b)
}

const explainSettingsSQL = `SELECT
    pg_catalog.set_config('statement_timeout', $1, true),
    pg_catalog.set_config('lock_timeout', $2, true),
    pg_catalog.set_config('plan_cache_mode', 'force_generic_plan', true),
    pg_catalog.set_config('jit', 'off', true)`

func (x *StatementExplainer) explainIn(ctx context.Context, tx pgx.Tx, name string,
	queryID int64, res probes.Result) probes.Result {
	if _, err := tx.Exec(ctx, explainSettingsSQL,
		fmt.Sprintf("%dms", probes.MaxStatementTimeout.Milliseconds()), "100ms"); err != nil {
		return readFailure(res, err)
	}
	known, err := statementKnown(ctx, tx, queryID)
	switch {
	case err != nil:
		return readFailure(res, err)
	case !known:
		res.Status, res.Reason = probes.StatusEmpty, ReasonUnknownStatement
		return res
	}
	var text string
	ok := false
	if x.texts != nil {
		text, ok = x.texts(queryID)
	}
	if !ok || strings.TrimSpace(text) == "" {
		return failedRead(res, probes.StatusUnsupported, ReasonTextNotCached, nil)
	}
	body, err := explain.ReadOnlyStatement(text)
	if err != nil {
		return failedRead(res, probes.StatusUnsupported, ReasonNotReadStatement, nil)
	}
	return runExplain(ctx, tx, name, body, res)
}

// statementKnown confirms the queryid on this database without loading
// query text (showtext => false); a missing pg_stat_statements is
// unsupported.
func statementKnown(ctx context.Context, tx pgx.Tx, queryID int64) (bool, error) {
	var schema string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT n.nspname FROM
		pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'pg_stat_statements'), '')`).Scan(&schema); err != nil {
		return false, err
	}
	if schema == "" {
		return false, fmt.Errorf("%w: pg_stat_statements is not installed",
			errExplainUnsupported)
	}
	var known bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{schema}.Sanitize()+
		`.pg_stat_statements(showtext => false) s
		WHERE s.queryid = $1 AND s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
		      WHERE d.datname = pg_catalog.current_database()))`, queryID).Scan(&known)
	return known, err
}

var errExplainUnsupported = errors.New("explain unsupported")

// runExplain prepares the statement (extended protocol: one statement
// only) on the application's search_path (security-01c-n) and explains
// its generic plan with NULL arguments, which a generic plan does not use.
func runExplain(ctx context.Context, tx pgx.Tx, name, body string,
	res probes.Result) probes.Result {
	if err := apppath.Pin(ctx, tx); err != nil {
		return readFailure(res, err)
	}
	prep := tx.Conn().PgConn().ExecParams(ctx, "PREPARE "+name+" AS "+body, nil, nil, nil,
		nil)
	if _, err := prep.Close(); err != nil {
		return explainFailure(res, err)
	}
	execute := "EXECUTE " + name
	if n := placeholders(body); n > 0 {
		execute += "(" + strings.TrimSuffix(strings.Repeat("NULL, ", n), ", ") + ")"
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+execute).Scan(&raw); err != nil {
		return explainFailure(res, err)
	}
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) == 0 {
		return failedRead(res, probes.StatusError, "unreadable_plan", err)
	}
	res.Columns = []string{"depth", "node_type", "relation", "index", "join_type",
		"startup_cost", "total_cost", "plan_rows", "plan_width"}
	planRows(plans[0].Plan, 0, &res.Rows)
	res.Truncated = len(res.Rows) >= maxPlanNodes
	res.Status = probes.StatusOK
	return res
}

// planRows flattens a plan tree depth-first into node-shape rows.
func planRows(node map[string]any, depth int, out *[]probes.Row) {
	if node == nil || len(*out) >= maxPlanNodes {
		return
	}
	row := probes.Row{"depth": int64(depth), "node_type": node["Node Type"],
		"relation": node["Relation Name"], "index": node["Index Name"],
		"join_type": node["Join Type"], "startup_cost": node["Startup Cost"],
		"total_cost": node["Total Cost"], "plan_rows": node["Plan Rows"],
		"plan_width": node["Plan Width"]}
	for k, v := range row {
		if v == nil {
			delete(row, k)
		}
	}
	*out = append(*out, row)
	children, _ := node["Plans"].([]any)
	for _, c := range children {
		child, _ := c.(map[string]any)
		planRows(child, depth+1, out)
	}
}

// readFailure types a failed read by the probe classification.
func readFailure(res probes.Result, err error) probes.Result {
	st, reason := probes.Classify(err)
	return failedRead(res, st, reason, err)
}

// explainFailure types a failed PREPARE or EXPLAIN: the probe
// classification, except that a missing privilege (42501: the role cannot
// read a relation the statement reads) is missing_select. A 42501 anywhere
// else (no CONNECT on the database, say) is not a missing SELECT.
func explainFailure(res probes.Result, err error) probes.Result {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return failedRead(res, probes.StatusNoPrivilege, ReasonMissingSelect, err)
	}
	return readFailure(res, err)
}

// failedRead marks res as not observed, with a bounded error.
func failedRead(res probes.Result, st probes.Status, reason string,
	err error) probes.Result {
	res.Status, res.Reason, res.Rows = st, reason, nil
	if errors.Is(err, errExplainUnsupported) {
		res.Status, res.Reason = probes.StatusUnsupported, "extension_not_installed"
	}
	if err != nil {
		res.Error = truncateRunes(RedactText(err.Error()), 200)
	}
	return res
}
