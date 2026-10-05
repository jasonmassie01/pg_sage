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

	"github.com/pg-sage/sidecar/internal/facts"
)

// DefaultStatementTimeout bounds each first-look statement unless the
// session already has a lower statement_timeout, which is kept.
const DefaultStatementTimeout = 5 * time.Second

// Options configure one first look.
type Options struct {
	Database, Provider string
	StatementTimeout   time.Duration
	Thresholds         Thresholds
	Now                func() time.Time
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
	pool    *pgxpool.Pool
	opts    Options
	tx      pgx.Tx
	timeout int
	report  *Report
	window  StatsWindow
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
	if err := p.open(ctx); err != nil {
		return Report{}, err
	}
	r.StatementTimeoutMS = p.timeout
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
	rules []string
	fn    func(context.Context, pgx.Tx) ([]outcome, error)
}

func (p *pass) steps() []step {
	return []step{
		{[]string{RuleInvalidIndex, RuleDuplicateIndex, RuleNeverScannedIndex,
			RuleUnindexedFK}, p.indexChecks},
		{[]string{RuleXIDRunway}, p.xid},
		{[]string{RuleSequenceRunway}, p.sequences},
		{[]string{RuleTableBloat}, p.bloat},
		{[]string{RuleTestSchema}, p.testSchemas},
		{[]string{RuleMissingExtension}, p.extensions},
	}
}

// open begins the read-only transaction and applies the statement timeout.
func (p *pass) open(ctx context.Context) error {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("first look: begin read-only transaction: %w", err)
	}
	p.tx = tx
	var raw string
	if err := tx.QueryRow(ctx, "SHOW statement_timeout").Scan(&raw); err != nil {
		return fmt.Errorf("first look: read statement_timeout: %w", err)
	}
	want := int(p.opts.StatementTimeout / time.Millisecond)
	if session := parseTimeoutMS(raw); session > 0 && session < want {
		p.timeout = session
		return nil
	}
	p.timeout = want
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
			return p.fail(ctx, st.rules, err)
		}
	}
	outcomes, err := st.fn(ctx, p.tx)
	if err != nil {
		p.close()
		return p.fail(ctx, st.rules, err)
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
		p.report.Checks = append(p.report.Checks, Check{Rule: o.rule, Status: status,
			Note: note})
	}
	return nil
}

// degradedPrefix marks an outcome note as a degraded check.
const degradedPrefix = "degraded: "

func (p *pass) fail(ctx context.Context, rules []string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("first look: %w", ctx.Err())
	}
	for _, rule := range rules {
		p.degrade(rule, err)
	}
	return nil
}

func (p *pass) degrade(rule string, err error) {
	p.report.Checks = append(p.report.Checks, Check{Rule: rule, Status: CheckDegraded,
		Note: degradeReason(err, p.timeout)})
}

// degradeReason states why a check could not run, with the fix when known.
func degradeReason(err error, timeoutMS int) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "57014":
			return fmt.Sprintf("statement timeout (%d ms) reached: %s", timeoutMS,
				pgErr.Message)
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

// parseTimeoutMS reads a SHOW statement_timeout value ("0", "250ms", "5s",
// "1min") in milliseconds; 0 means none or unparsable.
func parseTimeoutMS(raw string) int {
	raw = strings.TrimSpace(raw)
	units := []struct {
		suffix string
		ms     int
	}{{"ms", 1}, {"min", 60000}, {"s", 1000}, {"h", 3600000}, {"d", 86400000}}
	for _, u := range units {
		if strings.HasSuffix(raw, u.suffix) {
			n, err := strconv.Atoi(strings.TrimSuffix(raw, u.suffix))
			if err != nil {
				return 0
			}
			return n * u.ms
		}
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
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
