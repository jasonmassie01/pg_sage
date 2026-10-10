package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
	"github.com/pg-sage/sidecar/internal/explain"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// cursorName is the brokered cursor; the attribution view counts its
// DECLAREs in pg_stat_statements.
const cursorName = "sage_agent_q"

// defaultCloseTimeout bounds closing a connection that failed mid-call.
const defaultCloseTimeout = 5 * time.Second

// execute runs the statement on the principal's broker login (S0, S4).
func (c *call) execute(ctx context.Context, p agentguard.Principal, role, password string,
	read sqlast.BrokeredRead, rels []relation, v decide.Verdict) (Result, error) {
	conn, release, err := c.b.pools.acquire(ctx, c.t, p.ID, p.Name, role, password)
	if err != nil {
		return c.connectFailure(p, err)
	}
	defer release()
	if err := c.begin(ctx, conn.Conn().PgConn()); err != nil {
		closeBroken(conn)
		return Result{}, fmt.Errorf("%w: opening the read-only transaction: %v",
			ErrUnavailable, err)
	}
	defer c.rollback(conn)
	env := v.Env
	if env == "" {
		env = envbind.EnvProd
	}
	return c.inTransaction(ctx, conn, read, rels, env)
}

// connectFailure maps a failed broker login: a rejected password drops the
// pool (the credential rotated mid-flight) and is retryable.
func (c *call) connectFailure(p agentguard.Principal, err error) (Result, error) {
	if pgErr, ok := asPgError(err); ok && strings.HasPrefix(pgErr.Code, "28") {
		c.b.pools.drop(p.ID, c.t.Name)
	}
	c.b.deps.Log("WARN", "agent_query: broker login of %s on %s failed: %v", p.ID,
		c.t.Name, err)
	return Result{}, fmt.Errorf("%w: the broker login failed", ErrUnavailable)
}

// begin opens BEGIN READ ONLY with the broker's own timeouts and search
// path, set in this transaction so no statement can lift them (RO-15).
func (c *call) begin(ctx context.Context, pg *pgconn.PgConn) error {
	path := []string{"pg_catalog"}
	for _, s := range c.path {
		path = append(path, pgx.Identifier{s}.Sanitize())
	}
	path = append(path, "pg_temp")
	stmts := []string{"BEGIN READ ONLY",
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'",
			c.b.cfg.StatementTimeout.Milliseconds()),
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", c.b.cfg.LockTimeout.Milliseconds()),
		"SET LOCAL search_path = " + strings.Join(path, ", ")}
	for _, s := range stmts {
		if _, err := pg.ExecParams(ctx, s, nil, nil, nil, nil).Close(); err != nil {
			return err
		}
	}
	return nil
}

// rollback ends the transaction; a connection that cannot is closed so it
// never returns to the pool mid-transaction.
func (c *call) rollback(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), c.b.cfg.LockTimeout+
		c.b.cfg.StatementTimeout)
	defer cancel()
	pg := conn.Conn().PgConn()
	if _, err := pg.ExecParams(ctx, "ROLLBACK", nil, nil, nil, nil).Close(); err != nil {
		closeBroken(conn)
	}
}

func closeBroken(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCloseTimeout)
	defer cancel()
	_ = conn.Conn().Close(ctx)
}

// inTransaction proves, describes, plans the output, then runs the cursor.
func (c *call) inTransaction(ctx context.Context, conn *pgxpool.Conn,
	read sqlast.BrokeredRead, rels []relation, env envbind.Env) (Result, error) {
	refusal, err := explain.ProveRead(ctx, conn, read.Canonical, nil, c.b.proof)
	if err != nil {
		c.b.deps.Log("WARN", "agent_query: catalog proof failed for %s: %v", c.id.Principal.ID,
			err)
		return blocked(ReasonNotProven, "the statement could not be verified against the "+
			"catalog", "retry; if it persists, an operator checks pg_sage's logs"), nil
	}
	if refusal != "" {
		return blocked(ReasonNotProven, "the statement "+refusal,
			"read only through side-effect-free functions and allowed relations"), nil
	}
	pg := conn.Conn().PgConn()
	desc, err := pg.Prepare(ctx, "", read.Canonical, nil)
	if err != nil {
		return c.dbFailure(err)
	}
	plan, refused, err := c.plan(ctx, desc.Fields, rels, read, env)
	if err != nil || refused.Verdict != "" {
		return refused, err
	}
	return c.fetch(ctx, pg, read.Canonical, plan)
}

// plan classifies the output; a refusal comes back as a result.
func (c *call) plan(ctx context.Context, fields []pgconn.FieldDescription, rels []relation,
	read sqlast.BrokeredRead, env envbind.Env) (outputPlan, Result, error) {
	cl := &classifier{b: c.b, t: c.t, pid: c.id.Principal.ID, env: env,
		cache: map[uint32]classify.RelationClasses{}, views: map[uint32]classify.Class{},
		kinds: map[uint32]string{}}
	relids, oids := []uint32{}, []uint32{}
	for _, r := range rels {
		cl.kinds[r.OID] = r.Kind
		relids = append(relids, r.OID)
	}
	for _, f := range fields {
		relids, oids = append(relids, f.TableOID), append(oids, f.DataTypeOID)
	}
	names, err := attributeNames(ctx, c.t.Pool, relids)
	if err != nil {
		return outputPlan{}, Result{}, err
	}
	cl.names = names
	types, err := typeNames(ctx, c.t.Pool, oids)
	if err != nil {
		return outputPlan{}, Result{}, err
	}
	p := cl.planOutput(ctx, fields, rels, types)
	refusal := cl.maskedRefs(ctx, rels, read)
	if cl.errOut != nil {
		return p, Result{}, fmt.Errorf("%w: reading column classes: %v", ErrUnavailable,
			cl.errOut)
	}
	c.rec.Classes = p.classes
	reason := string(decide.ReasonClassification)
	switch {
	case len(p.denied) > 0:
		return p, blocked(reason, p.deniedDetail(), "ask an operator for an unmask "+
			"entry, or select other columns"), nil
	case refusal != "":
		return p, blocked(reason, refusal, "filter and sort on unmasked columns"), nil
	}
	return p, Result{}, nil
}

// fetch declares the cursor and fetches max_rows + 1 rows, the extra row
// signalling truncation, within the byte cap; the transaction then ends,
// so a large result is never drained (S4).
func (c *call) fetch(ctx context.Context, pg *pgconn.PgConn, canonical string,
	plan outputPlan) (Result, error) {
	declare := "DECLARE " + cursorName + " NO SCROLL CURSOR FOR " + canonical
	if _, err := pg.ExecParams(ctx, declare, c.params, nil, nil, nil).Close(); err != nil {
		return c.dbFailure(err)
	}
	rr := pg.ExecParams(ctx, fmt.Sprintf("FETCH %d FROM %s", c.maxRows+1, cursorName),
		nil, nil, nil, nil)
	res := Result{Verdict: VerdictExecute, Status: StatusOK, Columns: plan.columns,
		Rows: [][]*string{}, Masked: plan.masked}
	if res.Masked == nil {
		res.Masked = []string{}
	}
	bytes := 0
	for rr.NextRow() {
		row, size := renderRow(rr.Values(), plan.actions)
		if len(res.Rows) == c.maxRows || bytes+size > c.b.cfg.MaxBytes {
			res.Truncated = true
			break
		}
		bytes += size
		res.Rows = append(res.Rows, row)
	}
	if _, err := rr.Close(); err != nil {
		return c.dbFailure(err)
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// renderRow copies one row's text values, masking per the plan; size is
// the bytes the agent receives.
func renderRow(values [][]byte, actions []columnAction) ([]*string, int) {
	row := make([]*string, len(values))
	size := 0
	for i, v := range values {
		if v == nil {
			continue
		}
		s := string(v)
		if i < len(actions) && actions[i] == actMask {
			s = MaskedValue
		}
		row[i] = &s
		size += len(s)
	}
	return row, size
}

// dbFailure is a server error as the agent sees it; the full text goes to
// the operator log only.
func (c *call) dbFailure(err error) (Result, error) {
	pgErr, ok := asPgError(err)
	if !ok {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, err
		}
		return Result{}, fmt.Errorf("%w: the broker connection failed: %v", ErrUnavailable,
			err)
	}
	message := pgErr.Message
	if strings.HasPrefix(pgErr.Code, "22") {
		message = "(withheld: a data exception can quote column values)"
	}
	c.b.deps.Log("INFO", "agent_query by %s on %s failed: SQLSTATE %s: %s",
		c.id.Principal.ID, c.t.Name, pgErr.Code, message)
	f := sanitize(pgErr, c.b.cfg.StatementTimeout)
	return Result{Verdict: VerdictExecute, Status: StatusFailed, SQLState: f.SQLState,
		Message: f.Message, Retryable: f.Retryable, Columns: []Column{},
		Rows: [][]*string{}, Masked: []string{}}, nil
}
