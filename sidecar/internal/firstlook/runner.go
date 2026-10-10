package firstlook

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentposture"
	"github.com/pg-sage/sidecar/internal/facts"
)

// DefaultStatementTimeout is the first look's own budget for each
// statement. A lower statement_timeout the operator set (on the role, the
// database, the server or in the connection options) is kept; one pg_sage
// set on its own session is not.
const DefaultStatementTimeout = 5 * time.Second

// Options configure one first look.
type Options struct {
	Database, Provider string
	StatementTimeout   time.Duration
	// TimeoutSetBy names the operator setting StatementTimeout comes from,
	// for degraded notes; empty means the first look's own budget.
	TimeoutSetBy string
	Thresholds   Thresholds
	Now          func() time.Time
	// Posture is the agent posture configuration (agents.*); nil is
	// agentposture.DefaultConfig.
	Posture *agentposture.Config
	// PostureRegistry holds the posture detectors; nil is
	// agentposture.Default().
	PostureRegistry *agentposture.Registry
}

func (o Options) withDefaults() Options {
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = DefaultStatementTimeout
	}
	if o.Thresholds == (Thresholds{}) {
		o.Thresholds = DefaultThresholds()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// pass is one first look in progress: a read-only transaction that is
// reopened after a failed step, so one failing check degrades alone.
type pass struct {
	pool       *pgxpool.Pool
	opts       Options
	tx         pgx.Tx
	timeout    int
	setBy      string // who set timeout; empty is the first look's own budget
	report     *Report
	window     StatsWindow
	postureEnv postureEnv
}

// Run takes the first look of the database behind pool. Every check that
// fails (a timeout, a missing privilege) is reported degraded with the
// reason; Run itself fails only without a pool or when ctx ends.
func Run(ctx context.Context, pool *pgxpool.Pool, opts Options) (Report, error) {
	if pool == nil {
		return Report{}, ErrNoPool
	}
	if err := ctx.Err(); err != nil {
		return Report{}, fmt.Errorf("first look: %w", err)
	}
	opts = opts.withDefaults()
	r := &Report{Database: opts.Database, Provider: opts.Provider, StartedAt: opts.Now()}
	p := &pass{pool: pool, opts: opts, report: r}
	defer p.close()
	if err := p.openOrDegrade(ctx); err != nil {
		return Report{}, err
	}
	r.StatementTimeoutMS = p.limitMS()
	p.header(ctx)
	for _, st := range p.steps() {
		if err := p.run(ctx, st); err != nil {
			return Report{}, err
		}
	}
	sortItems(r.Items)
	r.FinishedAt = opts.Now()
	r.DurationMS = r.FinishedAt.Sub(r.StartedAt).Milliseconds()
	return *r, nil
}

// outcome is how one rule's check ended.
type outcome struct {
	rule string
	n    int
	note string
}

// step reads one part of the catalog and evaluates its rules; when the
// read fails every rule it covers is degraded.
type step struct {
	rules   []string
	section string
	fn      func(context.Context, pgx.Tx) ([]outcome, error)
}

func (p *pass) steps() []step {
	return append(p.catalogSteps(), p.postureSteps()...)
}

func (p *pass) catalogSteps() []step {
	return []step{
		{[]string{RuleInvalidIndex, RuleDuplicateIndex, RuleNeverScannedIndex,
			RuleUnindexedFK}, "", p.indexChecks},
		{[]string{RuleXIDRunway}, "", p.xid},
		{[]string{RuleSequenceRunway}, "", p.sequences},
		{[]string{RuleTableBloat}, "", p.bloat},
		{[]string{RuleTestSchema}, "", p.testSchemas},
		{[]string{RuleMissingExtension}, "", p.extensions},
	}
}

// open begins the read-only transaction and applies the statement timeout:
// the budget in opts, unless the operator set a lower one on the session.
func (p *pass) open(ctx context.Context) error {
	// Nothing bounds this transaction's first statements on the server yet, and a
	// catalog-cache rebuild can wait on a lock held by DDL: bound them here.
	ctx, cancel := context.WithTimeout(ctx, p.stepBudget())
	defer cancel()
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("first look: begin read-only transaction: %w", err)
	}
	p.tx = tx
	// The catalog reads are planned at costs JIT compiles for: hundreds of
	// ms of compilation for queries that run in tens.
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('jit', 'off', true)"); err != nil {
		return fmt.Errorf("first look: turn JIT off: %w", err)
	}
	var setting int
	var source string
	if err := tx.QueryRow(ctx, `SELECT setting::int, source FROM pg_catalog.pg_settings
		WHERE name = 'statement_timeout'`).Scan(&setting, &source); err != nil {
		return fmt.Errorf("first look: read statement_timeout: %w", err)
	}
	want := int(p.opts.StatementTimeout / time.Millisecond)
	if limit, by := operatorLimit(setting, source); limit > 0 && limit < want {
		p.timeout, p.setBy = limit, by
		return nil
	}
	p.timeout, p.setBy = want, p.opts.TimeoutSetBy
	if _, err := tx.Exec(ctx, "SELECT pg_catalog.set_config('statement_timeout', $1, true)",
		strconv.Itoa(want)); err != nil {
		return fmt.Errorf("first look: set statement_timeout: %w", err)
	}
	return nil
}

func (p *pass) close() {
	if p.tx != nil {
		_ = p.tx.Rollback(context.Background()) // read-only: nothing to keep
		p.tx = nil
	}
}

// run executes one step; a failure degrades its rules and reopens the
// transaction for the next step. Only the end of ctx stops the pass.
func (p *pass) run(ctx context.Context, st step) error {
	if p.tx == nil {
		if err := p.open(ctx); err != nil {
			return p.fail(ctx, st, err)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, p.stepBudget())
	outcomes, err := st.fn(sctx, p.tx)
	if err != nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
		// The deadline cancelled a query and pgx closed the connection: the
		// step may report that secondary error; the cause is the budget.
		err = fmt.Errorf("%w (%v)", context.DeadlineExceeded, err)
	}
	cancel()
	if err != nil {
		p.close()
		return p.fail(ctx, st, err)
	}
	for _, o := range outcomes {
		status := CheckOK
		if o.n > 0 {
			status = CheckFinding
		}
		note := o.note
		if strings.HasPrefix(note, degradedPrefix) {
			status, note = CheckDegraded, strings.TrimPrefix(note, degradedPrefix)
		}
		p.report.Checks = append(p.report.Checks, Check{Rule: o.rule, Section: st.section,
			Status: status, Note: note})
	}
	return nil
}

// degradedPrefix marks an outcome note as a degraded check.
const degradedPrefix = "degraded: "

func (p *pass) fail(ctx context.Context, st step, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("first look: %w", ctx.Err())
	}
	note := degradeReason(err, p.limitMS(), p.setBy)
	for _, rule := range st.rules {
		p.report.Checks = append(p.report.Checks, Check{Rule: rule, Section: st.section,
			Status: CheckDegraded, Note: note})
	}
	if retryableError(err) {
		p.report.Retryable = append(p.report.Retryable, st.rules...)
	}
	return nil
}

// openOrDegrade opens the pass's transaction. A failure other than the end
// of ctx is not fatal: each step tries to open again and is degraded with
// the reason if it still cannot.
func (p *pass) openOrDegrade(ctx context.Context) error {
	if err := p.open(ctx); err != nil {
		if ctx.Err() != nil {
			return err
		}
		p.close()
	}
	return nil
}

// limitMS is the statement timeout the pass runs under, or the one it
// would have set when the transaction could not be opened.
func (p *pass) limitMS() int {
	if p.timeout > 0 {
		return p.timeout
	}
	return int(p.opts.StatementTimeout / time.Millisecond)
}

// stepGrace is how long past twice the statement timeout pg_sage waits for
// a first-look step before cancelling it from the client side.
const stepGrace = time.Second

// stepBudget bounds one step from the client side: the server's
// statement_timeout normally ends a slow step first, but a wait outside a
// statement's timeout (a lock taken while opening the transaction) must
// not hang the first look.
func (p *pass) stepBudget() time.Duration {
	return 2*time.Duration(p.limitMS())*time.Millisecond + stepGrace
}

// degradeReason states why a check could not run, with the fix when known;
// a timeout names the operator setting it came from (setBy), if any.
func degradeReason(err error, timeoutMS int, setBy string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("did not finish within twice its %d ms statement timeout plus "+
			"a grace period (waiting on a catalog lock or a busy server); pg_sage "+
			"cancelled it", timeoutMS)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57014":
			limit := fmt.Sprintf("%d ms", timeoutMS)
			if setBy != "" {
				limit += ", " + setBy
			}
			return fmt.Sprintf("statement timeout (%s) reached: %s", limit, pgErr.Message)
		case "42501":
			return "permission denied: " + pgErr.Message + "; GRANT pg_monitor to the " +
				"pg_sage role"
		}
	}
	return err.Error()
}

// header reads the relation count and the statistics window; a failure
// leaves them unknown and is not a check of its own.
func (p *pass) header(ctx context.Context) {
	if p.tx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, p.stepBudget())
	defer cancel()
	if err := p.tx.QueryRow(ctx, relationsSQL).Scan(&p.report.Relations); err != nil {
		p.close()
		return
	}
	if w, err := readStatsWindow(ctx, p.tx); err == nil {
		p.window = w
	} else {
		p.close()
	}
}

// add keeps a rule's items (capped) and returns its outcome.
func (p *pass) add(rule string, items []Item) outcome {
	kept, dropped := capItems(items, p.opts.Thresholds.MaxItemsPerRule)
	p.report.Items = append(p.report.Items, kept...)
	o := outcome{rule: rule, n: len(kept)}
	if dropped > 0 {
		o.note = fmt.Sprintf("%d more not shown", dropped)
	}
	return o
}

// operatorLimit is the session's statement_timeout (pg_settings setting and
// source) when an operator set it, and where; 0 when none is set or when
// pg_sage set it on its own session (source "session"): the first look's
// budget replaces those. An unknown source counts as the operator's.
func operatorLimit(settingMS int, source string) (int, string) {
	if settingMS <= 0 || source == "session" || source == "default" {
		return 0, ""
	}
	where := map[string]string{
		"user":                 "set on the role",
		"database":             "set on the database",
		"database user":        "set on the role in this database",
		"configuration file":   "set in the server configuration",
		"command line":         "set in the server configuration",
		"environment variable": "set in the server configuration",
		"global":               "set in the server configuration",
		"client":               "set in the connection options",
	}[source]
	if where == "" {
		where = "source: " + source
	}
	return settingMS, "statement_timeout " + where
}

// fixtureProposals are the idle test schemas the fact store should hear
// about; the window must be known for "idle" to mean anything.
func fixtureProposals(ss []Schema, w StatsWindow, now time.Time) []facts.Proposal {
	if !w.Known {
		return nil
	}
	acts := make([]facts.SchemaActivity, len(ss))
	for i, s := range ss {
		acts[i] = facts.SchemaActivity{Name: s.Name, Tables: s.Tables, Activity: s.Activity}
	}
	return facts.IdleFixtureProposals(acts, now.Sub(w.Since), now)
}
