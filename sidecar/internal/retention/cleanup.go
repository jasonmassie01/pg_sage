package retention

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/rca"
)

// batchSize is the default number of rows one purge statement deletes.
const batchSize = 1000

// snapshotBatchSize bounds a snapshots purge statement: rows are TOAST
// documents of up to megabytes each (lifeos: 1.66 MB per indexes row), so
// a 1,000-row statement could free gigabytes of TOAST and WAL at once.
const snapshotBatchSize = 50

// defaultPause separates two purge statements, so a backlog is deleted
// as a trickle the database absorbs, not as one burst of WAL and I/O.
const defaultPause = 50 * time.Millisecond

// defaultRunBudget bounds one run. The orchestrator runs the cleaner in
// its cycle (about every 10 minutes, after the executor and the briefing),
// so a backlog is worked off over several runs instead of stalling the
// cycle; each run resumes where the last one stopped, and a rule that spent
// the budget goes last (deferFrom).
const defaultRunBudget = 30 * time.Second

// defaultRunInterval is used by RunEvery when no positive interval is given.
const defaultRunInterval = time.Hour

// Cleaner performs data retention cleanup of one database's sage schema.
// The fleet orchestrator calls Run once per cycle (see
// cmd/pg_sage_sidecar/fleet_orchestrator.go); RunEvery runs it on its own
// ticker. It is safe to construct one per monitored database.
type Cleaner struct {
	pool  *pgxpool.Pool
	cfg   *config.Config
	logFn func(string, string, ...any)
	// control holds the sre_* coordination and ledger tables when they
	// live in a meta database; nil means the monitored database.
	control *pgxpool.Pool

	pause  time.Duration // between purge statements
	budget time.Duration // per run
	next   int           // rule the next run starts with
	// capBytes overrides the snapshot size cap derived from the config
	// (tests); 0 derives it.
	capBytes int64
	// trimBudget bounds the snapshot documents one run trims for the cap
	// (cap_history.go).
	trimBudget int64
	// conv converts plain history tables in the background (convert.go).
	conv *conversions
	// notes rate-limits the size cap's warnings (cap_notes.go).
	notes *capNotes
}

// New creates a new retention Cleaner.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	logFn func(string, string, ...any),
) *Cleaner {
	return &Cleaner{pool: pool, cfg: cfg, logFn: logFn, pause: defaultPause,
		budget: defaultRunBudget, trimBudget: defaultTrimBudget, conv: &conversions{},
		notes: &capNotes{}}
}

// WithControlPool prunes the control-database tables (controlTables) in
// pool instead of the monitored database: the meta database in meta-db
// mode. Nil keeps the monitored database.
func (c *Cleaner) WithControlPool(pool *pgxpool.Pool) *Cleaner {
	c.control = pool
	return c
}

// WithPacing sets the pause between purge statements and the time budget
// of one run; a non-positive value keeps the default.
func (c *Cleaner) WithPacing(pause, budget time.Duration) *Cleaner {
	if pause > 0 {
		c.pause = pause
	}
	if budget > 0 {
		c.budget = budget
	}
	return c
}

// controlTables live beside the sre_* coordination tables (the control
// database), not necessarily in the monitored database.
var controlTables = map[string]bool{"sre_eval_runs": true}

// RunStats is what one run did.
type RunStats struct {
	Deleted     map[string]int64 // rows deleted per table
	Batches     map[string]int   // purge statements per table
	Statements  int              // purge statements in all
	Dropped     []string         // partitions dropped
	Deferred    []string         // tables left for the next run (budget spent)
	BudgetSpent bool
	Elapsed     time.Duration
}

func newRunStats() RunStats {
	return RunStats{Deleted: map[string]int64{}, Batches: map[string]int{}}
}

// count records one delete statement on table that removed n rows.
func (s *RunStats) count(table string, n int64) {
	s.Statements++
	s.Batches[table]++
	s.Deleted[table] += n
}

// Run starts the due background conversions of plain history tables
// (ConvertHistory) and performs one retention run (see RunOnce).
func (c *Cleaner) Run(ctx context.Context) {
	c.convertInBackground(ctx, time.Now())
	c.RunOnce(ctx)
}

// RunOnce purges expired data from every sage table, starting where the
// previous run stopped (deferFrom), until done or the run's budget is
// spent. A failing table is logged at ERROR and does not stop the others.
func (c *Cleaner) RunOnce(ctx context.Context) RunStats {
	start := time.Now()
	stats := newRunStats()
	deadline := start.Add(c.budget)
	rules := purgeRules(c.cfg)
	if c.next >= len(rules) {
		c.next = 0
	}
	first := c.next
	for i := 0; i < len(rules) && ctx.Err() == nil; i++ {
		at := (first + i) % len(rules)
		if i > 0 && time.Now().After(deadline) {
			c.deferFrom(rules, first, at, at, &stats) // rules[at] has not run: it goes first
			break
		}
		if !c.target(rules[at]).purge(ctx, rules[at], &stats, deadline) {
			// rules[at] spent the budget: it goes last next run, so a rule
			// that spends it every run cannot starve the others.
			c.deferFrom(rules, first, at, (at+1)%len(rules), &stats)
			break
		}
	}
	if !stats.BudgetSpent {
		c.next = 0
		if ctx.Err() == nil {
			c.pruneIncidents(ctx)
		}
	}
	stats.Elapsed = time.Since(start)
	return stats
}

// deferFrom records that the run, which started at rules[first], stopped at
// rules[at]: the rules from at up to first are left, and the next run
// starts at next. Every run starts at least one rule further on, so each
// rule runs within len(rules) runs, however long any one takes.
func (c *Cleaner) deferFrom(rules []purgeRule, first, at, next int, stats *RunStats) {
	stats.BudgetSpent = true
	c.next = next
	for i := 0; i < len(rules); i++ {
		idx := (at + i) % len(rules)
		if i > 0 && idx == first {
			break
		}
		stats.Deferred = append(stats.Deferred, rules[idx].table)
	}
	c.logFn("INFO", "retention: run budget of %s spent; %d rules deferred to the next run, "+
		"which starts with sage.%s", c.budget, len(stats.Deferred), rules[next].table)
}

// target is the cleaner that purges rule: the control database's for the
// control tables in meta-db mode.
func (c *Cleaner) target(rule purgeRule) *Cleaner {
	if controlTables[rule.table] && c.control != nil {
		t := *c
		t.pool = c.control
		return &t
	}
	return c
}

// pruneIncidents deletes incidents resolved more than findings_days ago
// through the RCA package's own hook, so the resolution-age rule lives in
// one place (substrate-B7). Open incidents are never deleted.
func (c *Cleaner) pruneIncidents(ctx context.Context) {
	days := c.cfg.Retention.FindingsDays
	if days <= 0 {
		return
	}
	window := time.Duration(days) * 24 * time.Hour
	deleted, err := rca.PruneResolvedIncidents(ctx, c.pool, window)
	if err != nil {
		c.logFn("ERROR", "retention: pruning resolved sage.incidents failed "+
			"after %d rows (retention: %d days): %v", deleted, days, err)
		return
	}
	if deleted > 0 {
		c.logFn("INFO", "retention: pruned %d resolved incidents "+
			"(retention: %d days)", deleted, days)
	}
}

// RunEvery runs the cleaner immediately and then every interval until
// ctx is cancelled. A non-positive interval uses one hour.
func (c *Cleaner) RunEvery(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultRunInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Run(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
