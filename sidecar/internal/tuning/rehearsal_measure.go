package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/workload"
)

// What a rehearsal measures of one statement on the clone: executed with
// EXPLAIN (ANALYZE, BUFFERS) only when it has no $n parameters and the
// analyze guard (explain.ProveRead) proves it can invoke nothing with a
// side effect; planned (EXPLAIN, a generic plan on PG16+) otherwise. Every
// measurement runs in a READ ONLY transaction on the application's
// search_path under the per-statement timeout, and is rolled back.

// notExecutedParams is why a parameterized statement is planned only.
const notExecutedParams = "not executed: parameters unknown"

// errAnalyzeTimedOut is an executed statement that hit the timeout: it
// records no cost and no blocks.
var errAnalyzeTimedOut = errors.New("timed out on the clone")

type measurer struct {
	conn    *pgx.Conn
	version int
	opts    RehearsalOptions
}

// measured is one statement's measurement: blocks is nil when it was not
// executed, and notExecuted then says why. plan is the EXPLAIN JSON and
// latencyMS the execution time of an executed statement.
type measured struct {
	cost        float64
	blocks      *int64
	notExecuted string
	plan        []byte
	latencyMS   float64
}

// before measures every statement without the index.
func (m measurer) before(ctx context.Context, stmts []RehearsalStatement) []RehearsedQuery {
	out := make([]RehearsedQuery, len(stmts))
	for i, s := range stmts {
		out[i].QueryID = s.QueryID
		got, err := m.measure(ctx, s.Text)
		// Why it was not executed holds even when planning failed too.
		out[i].NotExecuted = got.notExecuted
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		out[i].BeforeCost, out[i].BeforeBlocks = got.cost, got.blocks
	}
	return out
}

// after measures again with the index the statements measured before.
// Blocks are compared only when both runs executed.
func (m measurer) after(ctx context.Context, stmts []RehearsalStatement, qs []RehearsedQuery) {
	for i, s := range stmts {
		if qs[i].Error != "" {
			continue
		}
		got, err := m.measure(ctx, s.Text)
		if err != nil {
			qs[i].Error = err.Error()
			continue
		}
		qs[i].AfterCost = got.cost
		switch {
		case got.blocks == nil && qs[i].BeforeBlocks != nil:
			qs[i].BeforeBlocks, qs[i].NotExecuted = nil, got.notExecuted
		case qs[i].BeforeBlocks != nil:
			qs[i].AfterBlocks = got.blocks
		}
	}
}

// measure executes or plans one statement; see the file comment. A
// failure's server text can quote the clone's data, and it reaches the
// rehearsal result and the model, so a data exception or a constraint
// violation is withheld (explain.SafeError, security-10); its SQLSTATE is
// kept.
func (m measurer) measure(ctx context.Context, text string) (measured, error) {
	got, err := m.measureStatement(ctx, text)
	return got, explain.SafeError(err)
}

func (m measurer) measureStatement(ctx context.Context, text string) (measured, error) {
	body, ok := singleStatement(text)
	if !ok {
		return measured{}, errMultiStatement
	}
	if paramRef.MatchString(workload.BlankLiteralsAndComments(body)) {
		got, err := planOutside(ctx, m.conn, body, m.version)
		got.notExecuted = notExecutedParams
		return got, err
	}
	tx, err := m.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return measured{}, fmt.Errorf("begin the measurement on the clone: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := apppath.Pin(ctx, tx); err != nil {
		return measured{}, err
	}
	// The proof's catalog reads get at least DefaultAnalyzeTimeout each; the
	// analyze timeout is the executed statement's budget alone.
	if err := setStatementTimeout(ctx, tx, max(m.opts.AnalyzeTimeout,
		DefaultAnalyzeTimeout)); err != nil {
		return measured{}, err
	}
	refusal, err := explain.ProveRead(ctx, tx, body, m.opts.Inspect, explain.ReadProofOptions{})
	if err != nil {
		return measured{}, fmt.Errorf("prove the statement read-only on the clone: %w", err)
	}
	if refusal != "" {
		got, err := planIn(ctx, tx, body, m.version)
		got.notExecuted = "not executed: it " + refusal
		return got, err
	}
	if err := setStatementTimeout(ctx, tx, m.opts.AnalyzeTimeout); err != nil {
		return measured{}, err
	}
	return analyze(ctx, tx, body)
}

// setStatementTimeout bounds each later statement of tx by d.
func setStatementTimeout(ctx context.Context, tx pgx.Tx, d time.Duration) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)",
		fmt.Sprintf("%dms", d.Milliseconds())); err != nil {
		return fmt.Errorf("set the statement timeout on the clone: %w", err)
	}
	return nil
}

// planCost is the total cost of a statement's plan (planOutside).
func planCost(ctx context.Context, conn *pgx.Conn, text string, version int) (float64,
	error) {
	got, err := planOutside(ctx, conn, text, version)
	return got.cost, err
}

// planOutside plans a statement (EXPLAIN, never ANALYZE; a generic plan
// for a parameterized statement on PG16+) in a READ ONLY transaction on
// the application's search_path (security-01c-n) that is always rolled
// back.
func planOutside(ctx context.Context, conn *pgx.Conn, text string, version int) (measured,
	error) {
	body, ok := singleStatement(text)
	if !ok {
		return measured{}, errMultiStatement
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return measured{}, fmt.Errorf("begin the plan transaction on the clone: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := apppath.Pin(ctx, tx); err != nil {
		return measured{}, err
	}
	return planIn(ctx, tx, body, version)
}

// planIn plans body in tx, never executing it.
func planIn(ctx context.Context, tx pgx.Tx, body string, version int) (measured, error) {
	prefix := "EXPLAIN (FORMAT JSON) "
	if paramRef.MatchString(workload.BlankLiteralsAndComments(body)) {
		if version < 160000 {
			return measured{}, errNoGenericPlanning
		}
		prefix = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	raw, err := rawSingleValue(ctx, tx.Conn().PgConn(), prefix+body)
	if err != nil {
		return measured{}, timedOut(err)
	}
	doc, err := planRoot(raw)
	if err != nil {
		return measured{}, err
	}
	return measured{cost: doc.Plan.TotalCost, plan: raw}, nil
}

// analyze executes the proven statement once and reads its shared blocks
// and execution time.
func analyze(ctx context.Context, tx pgx.Tx, body string) (measured, error) {
	raw, err := rawSingleValue(ctx, tx.Conn().PgConn(),
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+body)
	if err != nil {
		return measured{}, timedOut(err)
	}
	doc, err := planRoot(raw)
	if err != nil {
		return measured{}, err
	}
	blocks := doc.Plan.SharedHit + doc.Plan.SharedRead
	return measured{cost: doc.Plan.TotalCost, blocks: &blocks, plan: raw,
		latencyMS: doc.ExecutionTime}, nil
}

// planNode is the root node of an EXPLAIN (FORMAT JSON): its buffer
// counts include its children's.
type planNode struct {
	TotalCost  float64 `json:"Total Cost"`
	SharedHit  int64   `json:"Shared Hit Blocks"`
	SharedRead int64   `json:"Shared Read Blocks"`
}

// planDoc is one EXPLAIN (FORMAT JSON) result: the root node and, for
// ANALYZE, the execution time in milliseconds.
type planDoc struct {
	Plan          planNode `json:"Plan"`
	ExecutionTime float64  `json:"Execution Time"`
}

func planRoot(raw []byte) (planDoc, error) {
	var plans []planDoc
	if err := json.Unmarshal(raw, &plans); err != nil {
		return planDoc{}, fmt.Errorf("unreadable plan: %w", err)
	}
	if len(plans) == 0 {
		return planDoc{}, errors.New("unreadable plan: no plan returned")
	}
	return plans[0], nil
}

// timedOut names a statement timeout (query_canceled) as such.
func timedOut(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return errAnalyzeTimedOut
	}
	return err
}
