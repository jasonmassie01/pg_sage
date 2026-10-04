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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// The investigator's safe EXPLAIN (roadmap 2.1). The model names a
// statement by its pg_stat_statements queryid; the text is read in the
// same read-only transaction and explained plan-only (never ANALYZE, so
// the statement never runs), only when it is one read statement, under
// the probe timeouts. It is prepared over the extended protocol (one
// statement only) and explained with plan_cache_mode force_generic_plan,
// so a normalized statement's $n parameters are planned generically on
// every supported version, never as constants. The evidence carries plan
// node shapes (node type, relation, index, join type, costs, row
// estimates), never the text or its predicates.

// ExplainProbeID is the evidence id of a plan-only EXPLAIN.
const ExplainProbeID probes.ID = "explain_statement"

// Explain refusals.
const (
	ReasonNotReadStatement = "not_a_read_statement"
	ReasonUnknownStatement = "unknown_statement"
)

// maxPlanNodes bounds the plan nodes one EXPLAIN keeps.
const maxPlanNodes = 40

// Explainer explains one statement, plan-only.
type Explainer interface {
	ExplainStatement(ctx context.Context, queryID int64) probes.Result
}

// StatementExplainer explains statements of one monitored database.
type StatementExplainer struct{ pool *pgxpool.Pool }

var _ Explainer = (*StatementExplainer)(nil)

// NewStatementExplainer explains statements of pool's database.
func NewStatementExplainer(pool *pgxpool.Pool) *StatementExplainer {
	return &StatementExplainer{pool: pool}
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
	res := probes.Result{ProbeID: ExplainProbeID, Version: "v1", ObservedAt: time.Now()}
	switch {
	case x == nil || x.pool == nil:
		return failedRead(res, probes.StatusError, "not_configured", nil)
	case queryID == 0:
		return failedRead(res, probes.StatusError, "invalid_args", nil)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, probes.MaxStatementTimeout+time.Second)
	defer cancel()
	conn, err := x.pool.Acquire(ctx)
	if err != nil {
		st, reason := probes.Classify(err)
		return failedRead(res, st, reason, err)
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		st, reason := probes.Classify(err)
		return failedRead(res, st, reason, err)
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
	res = explainIn(ctx, tx, name, queryID, res)
	res.ElapsedMS = time.Since(start).Milliseconds()
	return res
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

func explainIn(ctx context.Context, tx pgx.Tx, name string, queryID int64,
	res probes.Result) probes.Result {
	if _, err := tx.Exec(ctx, explainSettingsSQL,
		fmt.Sprintf("%dms", probes.MaxStatementTimeout.Milliseconds()), "100ms"); err != nil {
		st, reason := probes.Classify(err)
		return failedRead(res, st, reason, err)
	}
	text, err := statementText(ctx, tx, queryID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		res.Status, res.Reason = probes.StatusEmpty, ReasonUnknownStatement
		return res
	case err != nil:
		st, reason := probes.Classify(err)
		return failedRead(res, st, reason, err)
	}
	body, err := explain.ReadStatement(text)
	if err != nil {
		return failedRead(res, probes.StatusUnsupported, ReasonNotReadStatement, nil)
	}
	return runExplain(ctx, tx, name, body, res)
}

// statementText reads a statement's text; a missing pg_stat_statements
// is unsupported.
func statementText(ctx context.Context, tx pgx.Tx, queryID int64) (string, error) {
	var schema string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT n.nspname FROM
		pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'pg_stat_statements'), '')`).Scan(&schema); err != nil {
		return "", err
	}
	if schema == "" {
		return "", fmt.Errorf("%w: pg_stat_statements is not installed",
			errExplainUnsupported)
	}
	var text string
	err := tx.QueryRow(ctx, `SELECT s.query FROM `+pgx.Identifier{schema}.Sanitize()+
		`.pg_stat_statements(true) s
		WHERE s.queryid = $1 AND s.dbid = (SELECT d.oid FROM pg_catalog.pg_database d
		      WHERE d.datname = pg_catalog.current_database())
		ORDER BY s.calls DESC LIMIT 1`, queryID).Scan(&text)
	return text, err
}

var errExplainUnsupported = errors.New("explain unsupported")

// runExplain prepares the statement (extended protocol: one statement
// only) and explains its generic plan with NULL arguments, which a
// generic plan does not use.
func runExplain(ctx context.Context, tx pgx.Tx, name, body string,
	res probes.Result) probes.Result {
	prep := tx.Conn().PgConn().ExecParams(ctx, "PREPARE "+name+" AS "+body, nil, nil, nil,
		nil)
	if _, err := prep.Close(); err != nil {
		st, reason := probes.Classify(err)
		return failedRead(res, st, reason, err)
	}
	execute := "EXECUTE " + name
	if n := placeholders(body); n > 0 {
		execute += "(" + strings.TrimSuffix(strings.Repeat("NULL, ", n), ", ") + ")"
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+execute).Scan(&raw); err != nil {
		st, reason := probes.Classify(err)
		return failedRead(res, st, reason, err)
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
