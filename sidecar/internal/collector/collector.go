package collector

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/partition"
	"github.com/pg-sage/sidecar/internal/querystore"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/snapstore"
)

// Collector runs periodic stats collection against the target database.
type Collector struct {
	pool     *pgxpool.Pool
	cfg      *config.Config
	breaker  *CircuitBreaker
	mu       sync.RWMutex
	latest   *Snapshot
	previous *Snapshot
	// caps is what the database can report, re-derived each cycle
	// (guarded by mu); probeDB replaces the probe's querier (tests).
	caps       capState
	probeDB    catalogQuerier
	loadDB     catalogQuerier    // replaces the breaker's load querier (tests)
	loops      *selfbudget.Loops // replaces the process loop tracker (tests)
	health     health            // outcome record behind Status (status.go)
	logFn      func(string, string, ...any)
	snapWriter *snapstore.Writer
	// period is the ticker's period in nanoseconds (0 before Run); only
	// the Run goroutine resets the ticker, Interval reads it.
	period atomic.Int64
	// queries writes sage.query_store samples of moved queries only;
	// partitions creates the day partitions the writes land in.
	queries    *querystore.Recorder
	partitions *partition.Keeper
	// pins are the queryids sampled whatever their rank (watch.go).
	pins pinRegistry

	// skipConfigSnapshots is set before Run when no advisor will consume
	// the configuration snapshot.
	skipConfigSnapshots bool

	// stepOverrides replaces catalog category readers (tests).
	stepOverrides map[string]func(context.Context, *Snapshot) error

	// Bounded catalog reads (perf fix phase): caches and their tunables.
	catalogMu      sync.Mutex
	indexDefs      *indexDefCache
	sequences      sequenceCache
	seqCoverage    SequenceCoverage
	dbSize         dbSizeCache
	pgssClass      classCache
	stmtTexts      *stmtTextCache
	exactTopN      int           // relations sized exactly per cycle
	seqPageSize    int           // sequences per catalog transaction
	seqScanCap     int           // sequences read per cycle
	dbSizeEvery    time.Duration // pg_database_size cadence
	indexDefMaxAge time.Duration // full index-definition refresh backstop
	now            func() time.Time
	// onCatalogQuery runs inside each catalog transaction after its
	// statement (catalogread Reader.After; nil unless a test sets it).
	onCatalogQuery catalogHook
	// meter records what the catalog statements of the last cycles cost
	// (catalogread Reader.Done, wired by reader for every statement).
	meter costMeter
}

// Defaults for the bounded catalog reads.
const (
	// ExactSizeTopN relations (and as many indexes) get exact sizes each
	// cycle; every other size is relpages x block size.
	ExactSizeTopN = 100
	// SequencePageSize sequences are read per catalog transaction (each
	// holds one lock per sequence until the transaction ends); a page is
	// also capped at a quarter of the server's lock table.
	SequencePageSize = 1000
	// SequenceScanCap sequences are read per cycle at most; past it the
	// collector rotates through the catalog across cycles.
	SequenceScanCap = 20000
	// DBSizeRefreshInterval is how often pg_database_size (a stat() of
	// every file of the database) is measured.
	DBSizeRefreshInterval = 15 * time.Minute
	// IndexDefMaxAge forces a full index-definition refresh even when the
	// catalog update counters did not move (track_counts off).
	IndexDefMaxAge = time.Hour
)

// New creates a Collector wired to the given pool and config. pgVersionNum
// and the config's capability flags (the startup checks) only seed the
// capabilities each cycle re-derives.
func New(
	pool *pgxpool.Pool,
	cfg *config.Config,
	pgVersionNum int,
	logFn func(string, string, ...any),
) *Collector {
	return &Collector{
		pool: pool,
		cfg:  cfg,
		caps: capState{caps: seedCapabilities(pgVersionNum, cfg)},
		breaker: NewCircuitBreaker(
			cfg.Safety.CPUCeilingPct,
			cfg.Safety.BackoffConsecutiveSkips,
		),
		logFn:          logFn,
		snapWriter:     snapstore.NewWriter(),
		queries:        querystore.NewRecorder(),
		partitions:     partition.NewKeeper(),
		indexDefs:      newIndexDefCache(),
		stmtTexts:      newStmtTextCache(),
		exactTopN:      ExactSizeTopN,
		seqPageSize:    SequencePageSize,
		seqScanCap:     SequenceScanCap,
		dbSizeEvery:    DBSizeRefreshInterval,
		indexDefMaxAge: IndexDefMaxAge,
		now:            time.Now,
	}
}

// WithoutConfigSnapshots stops the advisor's per-cycle configuration
// snapshot (pg_settings and every table's reloptions). The runtime calls
// it, before Run, when no advisor will run, e.g. without a usable LLM.
func (c *Collector) WithoutConfigSnapshots() { c.skipConfigSnapshots = true }

// CollectsConfigSnapshots reports whether each cycle gathers the
// advisor's configuration snapshot.
func (c *Collector) CollectsConfigSnapshots() bool {
	return c.cfg.Advisor.Enabled && !c.skipConfigSnapshots
}

// Run starts the collection loop, blocking until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	interval := c.configuredInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.period.Store(int64(interval))

	c.logFn("INFO", "collector started, interval=%s", interval)

	// Collect at startup, not one interval later: the first snapshot feeds
	// the analyzer's first cycle within the first minute (five-minute time
	// to value).
	if ctx.Err() == nil {
		c.trackedCycle(ctx, ticker)
	}

	for {
		select {
		case <-ctx.Done():
			c.logFn("INFO", "collector stopped")
			return
		case <-ticker.C:
			c.trackedCycle(ctx, ticker)
		}
	}
}

// cycle collects and persists one snapshot. It returns why the cycle
// produced no stored snapshot (skipped, collection or persist failed),
// each already logged, so the loop tracker counts it (observability-06).
func (c *Collector) cycle(ctx context.Context, ticker intervalResetter) error {
	c.meter.start()
	if d := c.breaker.ShouldSkip(withStep(ctx, "load"), c.loadQuerier()); d.Skip {
		c.meter.abandon()
		c.skipCycle(ticker, d)
		return fmt.Errorf("%w: %s", ErrCycleSkipped, d.cause())
	}

	// Between cycles: follow a changed collector.interval_seconds, and
	// restore the normal interval if we were dormant.
	c.syncInterval(ticker)

	snap, err := c.collect(ctx)
	// A failed collection still spent its statements: it is metered.
	c.meter.finish(c.clock())
	if err != nil {
		c.noteError("collect", err)
		c.logFn("ERROR", "collection failed: %v", err)
		return fmt.Errorf("collect: %w", err)
	}

	c.mu.Lock()
	c.previous = c.latest
	c.latest = snap
	c.mu.Unlock()

	c.breaker.RecordSuccess()
	c.noteSuccess()

	persistErr := c.persist(ctx, snap)
	if persistErr != nil {
		c.notePersistFailure(persistErr)
		c.logFn("ERROR", "snapshot persist failed: %v", persistErr)
		persistErr = fmt.Errorf("persist snapshot: %w", persistErr)
	}

	c.recordQueryStore(ctx, snap)
	return persistErr
}

// skipCycle logs why the breaker skipped this cycle (load, or a failed
// load query: not load) and slows to the dormant interval once dormant.
func (c *Collector) skipCycle(ticker intervalResetter, d Decision) {
	if d.Err != nil {
		c.noteError("load query", d.Err)
	}
	c.logFn("WARN", "circuit breaker skip: %s", d.cause())
	if c.breaker.IsDormant() {
		c.logFn("WARN", "circuit breaker dormant, using dormant interval")
		c.usePeriod(ticker, c.cfg.Safety.DormantInterval())
	}
}

// loadQuerier runs the breaker's load query.
func (c *Collector) loadQuerier() catalogQuerier {
	if c.loadDB != nil {
		return c.loadDB
	}
	return timedCatalogQuerier{collector: c}
}

// LatestSnapshot returns the most recent snapshot (thread-safe).
func (c *Collector) LatestSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// PreviousSnapshot returns the snapshot before the latest (thread-safe).
func (c *Collector) PreviousSnapshot() *Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.previous
}

// collect gathers all stats categories into a single Snapshot. The
// catalog categories are read in order; one that fails is marked
// unavailable and logged, and the snapshot goes on (dogfood lifeos-1:
// an index query timing out on 35,439 indexes failed every snapshot).
// Only the system stats, the snapshot's spine, fail it.
func (c *Collector) collect(ctx context.Context) (*Snapshot, error) {
	now := time.Now().UTC()
	snap := &Snapshot{CollectedAt: now}
	c.refreshCapabilities(withStep(ctx, "capabilities"))

	// Read the epoch before the counters: a reset in between leaves the
	// old epoch on reset counters, which the counter-decrease check and
	// the next cycle's epoch change both expose.
	snap.StatsEpoch = c.collectStatementsEpoch(withStep(ctx, "stats_epoch"))
	c.stmtTexts.observeEpoch(snap.StatsEpoch)
	retries := 0
	for _, step := range c.catalogSteps() {
		err := c.runStep(ctx, step, snap, &retries)
		if err == nil {
			continue
		}
		if step.name == "system" {
			return nil, fmt.Errorf("collect system: %w", err)
		}
		c.markUnavailable(snap, step.name, err)
	}
	applyStatementTotals(snap)
	c.collectExtras(ctx, snap, &retries)
	// Collect pg_stat_statements.max for capacity monitoring.
	usage := withStep(ctx, "statements_usage")
	snap.System.StatStatementsMax = c.collectStatStatementsMax(usage)
	snap.System.StatStatements = c.collectStatStatementsUsage(usage,
		snap.System.StatStatementsMax)

	c.markSystemInterval(snap)
	c.markStatsReset(snap)
	return snap, nil
}

// collectExtras reads the non-fatal categories: a single replication-slot
// quirk (an unreserved slot, a hot standby) must not abort the snapshot,
// which would silently halt all stats collection (H1/H2). The extra steps
// spend the same cycle retry budget as the catalog steps.
func (c *Collector) collectExtras(ctx context.Context, snap *Snapshot, retries *int) {
	var err error
	if snap.Replication, err = c.collectReplication(withStep(ctx, "replication")); err != nil {
		c.logFn("WARN", "replication collection failed: %v", err)
		// Unknown, not "no replicas": unused-index drops depend on it.
		if snap.Unavailable == nil {
			snap.Unavailable = map[string]string{}
		}
		snap.Unavailable["replication"] = err.Error()
	}
	c.collectXminHoldersInto(withStep(ctx, "xmin_holders"), snap)
	for _, step := range c.extraSteps() {
		if err := c.runStep(ctx, step, snap, retries); err != nil {
			step.reset(snap)
			c.markUnavailable(snap, step.name, err)
		}
	}
}

// markUnavailable records that category could not be read this cycle: it
// is unknown, not empty, and the snapshot is kept without it.
func (c *Collector) markUnavailable(snap *Snapshot, category string, err error) {
	if snap.Unavailable == nil {
		snap.Unavailable = map[string]string{}
	}
	snap.Unavailable[category] = err.Error()
	c.logFn("WARN", "collector: %s unavailable this cycle (snapshot kept "+
		"without it): %v", category, err)
}

// markStatsReset flags snap when pg_stat_statements was reset since the
// previous snapshot: the statistics epoch changed (reliable even after
// counters regrew), or most shared counters fell sharply.
func (c *Collector) markStatsReset(snap *Snapshot) {
	c.mu.RLock()
	prev := c.latest
	c.mu.RUnlock()
	if prev == nil {
		return
	}
	if epochChanged(prev.StatsEpoch, snap.StatsEpoch) ||
		detectStatsReset(snap.Queries, prev.Queries) {
		snap.StatsReset = true
		c.logFn("WARN", "pg_stat_statements reset detected")
	}
}
