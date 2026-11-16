package tuning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
// executed, and notExecuted then says why.
type measured struct {
	cost        float64
	blocks      *int64
	notExecuted string
}

// before measures every statement without the index.
func (m measurer) before(ctx context.Context, stmts []RehearsalStatement) []RehearsedQuery {
	out := make([]RehearsedQuery, len(stmts))
	for i, s := range stmts {
		out[i].QueryID = s.QueryID
		got, err := m.measure(ctx, s.Text)
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		out[i].BeforeCost, out[i].BeforeBlocks = got.cost, got.blocks
		out[i].NotExecuted = got.notExecuted
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

// measure executes or plans one statement; see the file comment.
func (m measurer) measure(ctx context.Context, text string) (measured, error) {
	body, ok := singleStatement(text)
	if !ok {
		return measured{}, errMultiStatement
	}
	if paramRef.MatchString(workload.BlankLiteralsAndComments(body)) {
		cost, err := planCost(ctx, m.conn, body, m.version)
		return measured{cost: cost, notExecuted: notExecutedParams}, err
	}
	tx, err := m.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return measured{}, fmt.Errorf("begin the measurement on the clone: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := apppath.Pin(ctx, tx); err != nil {
		return measured{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true)",
		fmt.Sprintf("%dms", m.opts.AnalyzeTimeout.Milliseconds())); err != nil {
		return measured{}, fmt.Errorf("set the statement timeout on the clone: %w", err)
	}
	refusal, err := explain.ProveRead(ctx, tx, body, m.opts.Inspect, explain.ReadProofOptions{})
	if err != nil {
		return measured{}, fmt.Errorf("prove the statement read-only on the clone: %w", err)
	}
	if refusal != "" {
		cost, err := planIn(ctx, tx, body, m.version)
		return measured{cost: cost, notExecuted: "not executed: it " + refusal}, err
	}
	return analyze(ctx, tx, body)
}

// planCost is the total cost of a statement's plan (EXPLAIN, never
// ANALYZE; a generic plan for a parameterized statement on PG16+), planned
// in a READ ONLY transaction on the application's search_path
// (security-01c-n) that is always rolled back.
func planCost(ctx context.Context, conn *pgx.Conn, text string, version int) (float64,
	error) {
	body, ok := singleStatement(text)
	if !ok {
		return 0, errMultiStatement
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, fmt.Errorf("begin the plan transaction on the clone: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := apppath.Pin(ctx, tx); err != nil {
		return 0, err
	}
	return planIn(ctx, tx, body, version)
}

// planIn plans body in tx, never executing it.
func planIn(ctx context.Context, tx pgx.Tx, body string, version int) (float64, error) {
	prefix := "EXPLAIN (FORMAT JSON) "
	if paramRef.MatchString(workload.BlankLiteralsAndComments(body)) {
		if version < 160000 {
			return 0, errNoGenericPlanning
		}
		prefix = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	raw, err := rawSingleValue(ctx, tx.Conn().PgConn(), prefix+body)
	if err != nil {
		return 0, timedOut(err)
	}
	root, err := planRoot(raw)
	return root.TotalCost, err
}

// analyze executes the proven statement once and reads its shared blocks.
func analyze(ctx context.Context, tx pgx.Tx, body string) (measured, error) {
	raw, err := rawSingleValue(ctx, tx.Conn().PgConn(),
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+body)
	if err != nil {
		return measured{}, timedOut(err)
	}
	root, err := planRoot(raw)
	if err != nil {
		return measured{}, err
	}
	blocks := root.SharedHit + root.SharedRead
	return measured{cost: root.TotalCost, blocks: &blocks}, nil
}

// planNode is the root node of an EXPLAIN (FORMAT JSON): its buffer
// counts include its children's.
type planNode struct {
	TotalCost  float64 `json:"Total Cost"`
	SharedHit  int64   `json:"Shared Hit Blocks"`
	SharedRead int64   `json:"Shared Read Blocks"`
}

func planRoot(raw []byte) (planNode, error) {
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil {
		return planNode{}, fmt.Errorf("unreadable plan: %w", err)
	}
	if len(plans) == 0 {
		return planNode{}, errors.New("unreadable plan: no plan returned")
	}
	return plans[0].Plan, nil
}

// timedOut names a statement timeout (query_canceled) as such.
func timedOut(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return errAnalyzeTimedOut
	}
	return err
}
