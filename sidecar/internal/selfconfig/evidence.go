package selfconfig

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/extschema"
)

// Measure is one piece of evidence; Known is false when it was not read.
type Measure struct {
	Value float64 `json:"value"`
	Known bool    `json:"known"`
}

// Known is a measured value.
func Known(v float64) Measure { return Measure{Value: v, Known: true} }

// Evidence is what a derivation rests on, per database.
type Evidence struct {
	At                 time.Time
	Relations          Measure // pg_class rows
	CatalogScanMs      Measure // one pass over pg_class with its statistics
	Sequences          Measure // pg_sequences rows
	SequenceScanMs     Measure // reading every last value (a timed sample, scaled)
	MaxConnections     Measure // the server's max_connections
	TempBytes          Measure // pg_stat_database.temp_bytes (cumulative)
	StatsAgeSeconds    Measure // since the statistics reset (or server start)
	TempBytesPerSecond Measure // since the reset, or since the previous sample
	// StatementsScanMs: reading this database's pg_stat_statements entries
	// with their text, as the collector does each cycle (unknown without
	// the extension).
	StatementsScanMs Measure
	// CollectorCycleMs estimates one collector cycle's database time: the
	// catalog scan plus the statements read. Both are per cycle, so the
	// estimate does not grow with the interval it is used to size.
	CollectorCycleMs Measure
	// TempWindowPeakBytes is the largest temp-file growth the reactive
	// detector measured within one detector window since the previous pass
	// (unknown when the detector did not poll).
	TempWindowPeakBytes Measure
	// What the collector's own catalog statements spent over its last
	// cycles (evidence_collector.go; unknown before its first cycle):
	// MeasuredCycleMs is the p90 of each cycle's summed statement time,
	// SlowestCatalogMs the p90 of each cycle's slowest statement,
	// SlowestCatalogStep the step that ran the window's slowest one and
	// CatalogTimeouts the statements cut off by a server timeout.
	MeasuredCycleMs    Measure
	SlowestCatalogMs   Measure
	SlowestCatalogStep string
	CatalogTimeouts    Measure
	// Source is where the cost evidence came from: SourceCollector once
	// the collector's measurements are merged, otherwise SourceProbe.
	Source string
}

// sequenceSampleSize is how many last values the sequence evidence reads;
// the scan time is scaled to the sequence count.
const sequenceSampleSize = 250

// evidenceDeadline bounds each evidence read; a scan that hits it counts
// as at least this long.
const evidenceDeadline = 5 * time.Second

const (
	// CatalogScanSQL is the catalog-size read (exported: the performance
	// gate proves a live derivation pass ran by finding it). It reads a
	// statistics column: without one the planner replaces every
	// pg_stat_get_* output of the view with NULL and the probe would time
	// a bare pg_class scan (footprint-05, EXPLAIN VERBOSE on PG14 and PG17).
	// No literal: pg_stat_statements keeps the text as written, which the
	// performance gate matches.
	CatalogScanSQL = `/* pg_sage */ SELECT count(*)::float8, count(s.n_live_tup)::float8
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid`
	// sequenceCountSQL counts what pg_sequences lists without reading a
	// last value; sequenceSampleSQL reads $1 of them.
	sequenceCountSQL = `/* pg_sage */ SELECT count(*)::float8
FROM pg_catalog.pg_sequence s
JOIN pg_catalog.pg_class c ON c.oid = s.seqrelid
WHERE NOT pg_catalog.pg_is_other_temp_schema(c.relnamespace)`
	sequenceSampleSQL = `/* pg_sage */ SELECT count(*)::float8, count(last_value)::float8
FROM (SELECT last_value FROM pg_catalog.pg_sequences LIMIT $1) s`
	// statementsScanSQL + the qualified view + statementsScanWhere.
	statementsScanSQL = `/* pg_sage */ SELECT count(*)::float8,
       coalesce(sum(length(query)), 0)::float8
FROM `
	statementsScanWhere = `
WHERE dbid = (SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database())`
	maxConnectionsSQL = `/* pg_sage */ SELECT current_setting('max_connections')::float8`
	tempSQL           = `/* pg_sage */ SELECT temp_bytes::float8,
       extract(epoch FROM now() - coalesce(stats_reset, pg_postmaster_start_time()))::float8
FROM pg_catalog.pg_stat_database WHERE datname = current_database()`
)

// Gather reads the evidence from the database: each read in its own
// bounded read-only transaction. A read that fails leaves its measure
// unknown and is reported in the joined error; what was read is returned.
func Gather(ctx context.Context, pool *pgxpool.Pool) (Evidence, error) {
	ev := Evidence{At: time.Now(), Source: SourceProbe}
	if pool == nil {
		return ev, errors.New("selfconfig evidence: no database connection")
	}
	r := catalogread.New(pool, catalogread.Timeouts{Statement: evidenceDeadline,
		Lock: 100 * time.Millisecond})
	var errs []error
	note := func(name string, err error) {
		if err != nil {
			errs = append(errs, fmt.Errorf("selfconfig evidence %s: %w", name, err))
		}
	}
	note("catalog scan", ev.readCatalog(ctx, r))
	note("sequence scan", ev.readSequences(ctx, r))
	note("statements scan", ev.readStatements(ctx, r))
	if ev.CatalogScanMs.Known {
		ev.CollectorCycleMs = Known(ev.CatalogScanMs.Value + ev.StatementsScanMs.Value)
	}
	note("max_connections", ev.readMaxConnections(ctx, r))
	note("temp counters", ev.readTemp(ctx, r))
	return ev, errors.Join(errs...)
}

func (ev *Evidence) readCatalog(ctx context.Context, r catalogread.Reader) error {
	var n float64
	ms, err := timed(func() error {
		return r.QueryRow(ctx, CatalogScanSQL).Scan(&n, new(float64))
	})
	if timedOut(err) {
		ev.CatalogScanMs = Known(float64(evidenceDeadline.Milliseconds()))
		return nil
	}
	if err != nil {
		return err
	}
	ev.Relations, ev.CatalogScanMs = Known(n), Known(ms)
	return nil
}

func (ev *Evidence) readSequences(ctx context.Context, r catalogread.Reader) error {
	var n, sampled float64
	if err := r.QueryRow(ctx, sequenceCountSQL).Scan(&n); err != nil {
		return err
	}
	ms, err := timed(func() error {
		return r.QueryRow(ctx, sequenceSampleSQL, sequenceSampleSize).
			Scan(&sampled, new(float64))
	})
	if timedOut(err) {
		ev.Sequences = Known(n)
		ev.SequenceScanMs = Known(float64(evidenceDeadline.Milliseconds()))
		return nil
	}
	if err != nil {
		return err
	}
	ev.Sequences = Known(n)
	ev.SequenceScanMs = Known(extrapolateSequenceScan(ms, sampled, n))
	return nil
}

// extrapolateSequenceScan scales the time to read sampled last values to
// all total sequences: each read opens one sequence, so the cost is linear.
func extrapolateSequenceScan(ms, sampled, total float64) float64 {
	switch {
	case ms <= 0 || total <= 0:
		return 0
	case sampled <= 0 || total <= sampled:
		return ms
	}
	return ms * total / sampled
}

// readStatements times the statements read; a missing or unloaded
// pg_stat_statements is not an error (the collector skips it too).
func (ev *Evidence) readStatements(ctx context.Context, r catalogread.Reader) error {
	var n, text float64
	ms, err := timed(func() error {
		rel, err := extschema.StatementsRel(ctx, r, "pg_stat_statements")
		if err != nil {
			return err
		}
		return r.QueryRow(ctx, statementsScanSQL+rel+statementsScanWhere).Scan(&n, &text)
	})
	switch {
	case timedOut(err):
		ev.StatementsScanMs = Known(float64(evidenceDeadline.Milliseconds()))
		return nil
	case statementsMissing(err):
		return nil
	case err != nil:
		return err
	}
	ev.StatementsScanMs = Known(ms)
	return nil
}

// statementsMissing: the view does not exist (42P01) or the library is
// not preloaded (55000).
func statementsMissing(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "55000")
}

func (ev *Evidence) readMaxConnections(ctx context.Context, r catalogread.Reader) error {
	var n float64
	if err := r.QueryRow(ctx, maxConnectionsSQL).Scan(&n); err != nil {
		return err
	}
	ev.MaxConnections = Known(n)
	return nil
}

func (ev *Evidence) readTemp(ctx context.Context, r catalogread.Reader) error {
	var bytes, age float64
	if err := r.QueryRow(ctx, tempSQL).Scan(&bytes, &age); err != nil {
		return err
	}
	ev.TempBytes, ev.StatsAgeSeconds = Known(bytes), Known(age)
	if age > 0 {
		ev.TempBytesPerSecond = Known(bytes / age)
	}
	return nil
}

func timed(fn func() error) (float64, error) {
	start := time.Now()
	err := fn()
	return float64(time.Since(start).Microseconds()) / 1000, err
}

// timedOut reports a statement cancelled by its own deadline (57014).
func timedOut(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

// WithTempRateSince measures the temp-file rate since the previous sample
// when both counters are known, time passed and the counter did not reset;
// otherwise the since-reset rate stays.
func (ev Evidence) WithTempRateSince(prev Evidence) Evidence {
	if !ev.TempBytes.Known || !prev.TempBytes.Known || prev.At.IsZero() {
		return ev
	}
	dt := ev.At.Sub(prev.At).Seconds()
	delta := ev.TempBytes.Value - prev.TempBytes.Value
	if dt <= 0 || delta < 0 {
		return ev
	}
	ev.TempBytesPerSecond = Known(delta / dt)
	return ev
}

// WithTempPeak adds the detector's peak window (take: nil or false when
// there is no detector reading).
func (ev Evidence) WithTempPeak(take func() (float64, bool)) Evidence {
	if take == nil {
		return ev
	}
	if bytes, ok := take(); ok {
		ev.TempWindowPeakBytes = Known(bytes)
	}
	return ev
}

// namedMeasure is one citable measure; source names where a cost measure
// came from (probe or collector) and detail adds context (a step name).
type namedMeasure struct {
	name, unit     string
	m              Measure
	source, detail string
}

func (ev Evidence) measures() []namedMeasure {
	out := []namedMeasure{
		{name: "relations", unit: "relations", m: ev.Relations},
		{name: "catalog_scan_ms", unit: "ms", m: ev.CatalogScanMs, source: SourceProbe},
		{name: "sequences", unit: "sequences", m: ev.Sequences},
		{name: "sequence_scan_ms", unit: "ms", m: ev.SequenceScanMs, source: SourceProbe},
		{name: "statements_scan_ms", unit: "ms", m: ev.StatementsScanMs, source: SourceProbe},
		{name: "max_connections", unit: "connections", m: ev.MaxConnections},
		{name: "temp_bytes", unit: "bytes", m: ev.TempBytes},
		{name: "stats_age_seconds", unit: "s", m: ev.StatsAgeSeconds},
		{name: "temp_bytes_per_second", unit: "bytes/s", m: ev.TempBytesPerSecond},
		{name: "collector_cycle_ms", unit: "ms", m: ev.CollectorCycleMs, source: SourceProbe},
		{name: "temp_window_peak_bytes", unit: "bytes", m: ev.TempWindowPeakBytes},
	}
	return append(out, ev.collectorMeasures()...)
}

// Citations lists every known measure.
func (ev Evidence) Citations() []Citation { return ev.Cite() }

// Cite lists the named known measures (all known ones when none named).
func (ev Evidence) Cite(names ...string) []Citation {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := []Citation{}
	for _, nm := range ev.measures() {
		if nm.m.Known && usable(nm.m) && (len(want) == 0 || want[nm.name]) {
			out = append(out, Citation{Name: nm.name, Value: nm.m.Value, Unit: nm.unit,
				Source: nm.source, Detail: nm.detail})
		}
	}
	return out
}
