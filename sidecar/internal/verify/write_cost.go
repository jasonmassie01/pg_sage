package verify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/snapstore"
)

// The write cost of a verified index is measured on its own table
// (executor-03b): the cluster's WAL bytes per tuple the table wrote
// (pg_stat_user_tables n_tup_ins + n_tup_upd + n_tup_del), from the
// collector's cycles. WAL is cluster-wide, so an interval is charged to the
// table only when the table wrote at least writeDominance of the tuples
// the database wrote in it; an interval in which other tables wrote is
// unattributed, never guessed. Each window's cost is the median of its
// attributable intervals, robust to a burst from another database of the
// cluster.
const (
	// writeDominance is the table's minimum share of the database's
	// written tuples for an interval to be attributable.
	writeDominance = 0.9
	// minIntervalTuples is the fewest table tuples an attributable interval
	// holds: below it the collector's own background WAL dominates.
	minIntervalTuples = 100
	// minWriteIntervals and minWriteTuples are the attributable evidence
	// each window needs before the two windows are compared.
	minWriteIntervals = 3
	minWriteTuples    = 1000
	// maxWriteCycles bounds the collector cycles one window reads (picked
	// evenly, first and last included).
	maxWriteCycles = 49
	// writeCostTimeout bounds the history read of one window.
	writeCostTimeout = 30 * time.Second
)

// WriteCost is one window's table-scoped write cost. Intervals counts the
// attributable intervals, Unattributed the readable ones the table did not
// dominate (or barely wrote in), Unknown those whose readings cannot be
// compared (a statistics reset, no WAL position, no tables snapshot).
// Tuples, WALBytes and Seconds sum the attributable intervals;
// BytesPerTuple is their median WAL bytes per written tuple.
type WriteCost struct {
	Intervals     int     `json:"intervals"`
	Unattributed  int     `json:"unattributed"`
	Unknown       int     `json:"unknown"`
	Tuples        int64   `json:"tuples"`
	WALBytes      int64   `json:"wal_bytes"`
	Seconds       float64 `json:"seconds"`
	BytesPerTuple float64 `json:"bytes_per_tuple"`
}

// writeCycle is one collector cycle's readings: the cluster WAL insert
// position (nil on a standby or when unread), the relation statistics
// epoch, and the tuples written so far to the table and to every user
// table of the database (nil without a tables snapshot or the table in it).
type writeCycle struct {
	At             time.Time
	LSN            *int64
	Epoch          string
	TableTuples    *int64
	DatabaseTuples *int64
}

// summarizeWriteCycles differences consecutive cycles into intervals.
func summarizeWriteCycles(cycles []writeCycle) WriteCost {
	var cost WriteCost
	var perTuple []float64
	for i := 1; i < len(cycles); i++ {
		wal, table, database, ok := cycleDelta(cycles[i-1], cycles[i])
		switch {
		case !ok:
			cost.Unknown++
		case table < minIntervalTuples || float64(table) < writeDominance*float64(database):
			cost.Unattributed++
		default:
			cost.Intervals++
			cost.Tuples += table
			cost.WALBytes += wal
			cost.Seconds += cycles[i].At.Sub(cycles[i-1].At).Seconds()
			perTuple = append(perTuple, float64(wal)/float64(table))
		}
	}
	if len(perTuple) > 0 {
		cost.BytesPerTuple = median(perTuple)
	}
	return cost
}

// cycleDelta is what an interval wrote. ok is false when the two cycles
// cannot be compared: a reading is missing, the epoch is unknown or moved,
// a counter fell under the same epoch (a reset it did not record), or the
// WAL position went back (a failover to a server behind).
func cycleDelta(prev, cur writeCycle) (wal, table, database int64, ok bool) {
	if prev.LSN == nil || cur.LSN == nil || prev.TableTuples == nil ||
		cur.TableTuples == nil || prev.DatabaseTuples == nil || cur.DatabaseTuples == nil ||
		prev.Epoch == "" || prev.Epoch != cur.Epoch {
		return 0, 0, 0, false
	}
	wal = *cur.LSN - *prev.LSN
	table = *cur.TableTuples - *prev.TableTuples
	database = *cur.DatabaseTuples - *prev.DatabaseTuples
	if wal < 0 || table < 0 || database < 0 {
		return 0, 0, 0, false
	}
	return wal, table, database, true
}

// Write signal outcomes.
const (
	writeRegressed       = "regressed"
	writeWithinThreshold = "within_threshold"
	writeInsufficient    = "insufficient"
)

// writeJudgement compares the post-action write cost with the pre-action
// one. DeltaPct is set only when Measured.
type writeJudgement struct {
	Outcome  string
	Measured bool
	DeltaPct float64
	Reason   string
}

// judgeWrites reports a regression only for a rise strictly above
// thresholdPct between two windows that both hold enough attributable
// evidence; anything less is insufficient, never a regression.
func judgeWrites(thresholdPct float64, before, after WriteCost) writeJudgement {
	for _, side := range []struct {
		name string
		cost WriteCost
	}{{"pre-action", before}, {"post-action", after}} {
		if side.cost.Intervals < minWriteIntervals || side.cost.Tuples < minWriteTuples {
			return writeJudgement{Outcome: writeInsufficient, Reason: fmt.Sprintf(
				"%s window has %d attributable table-write intervals and %d tuples "+
					"(needs %d and %d)", side.name, side.cost.Intervals, side.cost.Tuples,
				minWriteIntervals, minWriteTuples)}
		}
	}
	if before.BytesPerTuple <= 0 {
		return writeJudgement{Outcome: writeInsufficient,
			Reason: "pre-action window has no attributable WAL per written tuple"}
	}
	if reason := writeRateIncomparable(thresholdPct, before, after); reason != "" {
		return writeJudgement{Outcome: writeInsufficient, Reason: reason}
	}
	judgement := writeJudgement{Outcome: writeWithinThreshold, Measured: true,
		DeltaPct: (after.BytesPerTuple - before.BytesPerTuple) * 100 / before.BytesPerTuple}
	if after.BytesPerTuple*100 > before.BytesPerTuple*(100+thresholdPct) {
		judgement.Outcome = writeRegressed
	}
	return judgement
}

// writeRateIncomparable is why the two windows' WAL per tuple cannot be
// compared, or "". Part of the WAL is paid per unit of time, not per tuple:
// full-page images after each checkpoint, the collector's own history
// writes. When the table writes more slowly after the action, that part
// spreads over fewer tuples and the per-tuple cost rises with no index cost
// at all. A post-action rate of at least 100/(100+thresholdPct) of the
// pre-action rate cannot by itself raise the cost above thresholdPct, even
// if all pre-action WAL were time-paid; a slower one is insufficient.
func writeRateIncomparable(thresholdPct float64, before, after WriteCost) string {
	if before.Seconds <= 0 || after.Seconds <= 0 {
		return "a window's attributable intervals have no duration"
	}
	// afterRate*(100+pct) >= beforeRate*100, without dividing.
	if float64(after.Tuples)*before.Seconds*(100+thresholdPct) >=
		float64(before.Tuples)*after.Seconds*100 {
		return ""
	}
	beforeRate := float64(before.Tuples) / before.Seconds
	afterRate := float64(after.Tuples) / after.Seconds
	return fmt.Sprintf("the table wrote %.1f tuples/s after the action and %.1f before; "+
		"at a rate that much lower the time-paid WAL (checkpoint full-page images, the "+
		"collector's writes) alone can raise the per-tuple cost over the threshold",
		afterRate, beforeRate)
}

// WriteCost reads the write cost of table over [from, to]: the table is
// named on the monitored database (to_regclass, so a quoted or search_path
// name resolves as the executor wrote it), the cycles are read from the
// history store.
func (s *PostgresObservationSource) WriteCost(
	ctx context.Context, table string, from, to time.Time,
) (WriteCost, error) {
	if s == nil || s.queryer == nil {
		return WriteCost{}, errors.New("verify observation pool is unavailable")
	}
	var schema, relname string
	err := s.queryer.QueryRow(ctx, `SELECT n.nspname::text, c.relname::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = to_regclass($1)`, table).Scan(&schema, &relname)
	if errors.Is(err, pgx.ErrNoRows) {
		return WriteCost{}, fmt.Errorf("resolve table %q: not found", table)
	}
	if err != nil {
		return WriteCost{}, fmt.Errorf("resolve table %q: %w", table, err)
	}
	cycles, err := s.writeCycles(ctx, schema, relname, from, to)
	if err != nil {
		return WriteCost{}, fmt.Errorf("read write cycles of %q: %w", table, err)
	}
	return summarizeWriteCycles(cycles), nil
}

// writeCyclesSQL reads at most $5 collector cycles of [$1, $2], picked
// evenly with the first and the last: each cycle's WAL position, statistics
// epoch, and the tuples written to the table ($3.$4) and to every user table
// in its tables snapshot. Only the picked cycles' tables documents are
// decoded.
var writeCyclesSQL = `/* pg_sage */
WITH cycles AS (
    SELECT sn.collected_at,
           CASE WHEN COALESCE((sn.data->>'is_replica')::boolean, false) THEN NULL
                ELSE (sn.data->>'wal_lsn')::int8 END AS lsn,
           COALESCE(sn.data->>'relation_stats_epoch', '') AS epoch,
           row_number() OVER (ORDER BY sn.collected_at, sn.id) AS n,
           count(*) OVER () AS total
      FROM sage.snapshots sn
     WHERE {db:sn} AND sn.category = 'system' AND sn.collected_at BETWEEN $1 AND $2
), picked AS (
    SELECT * FROM cycles
     WHERE n = total
        OR (n - 1) % GREATEST(ceil((total - 1)::float8 / ($5::int - 1)), 1)::int8 = 0
)
SELECT p.collected_at, p.lsn, p.epoch, w.table_tuples, w.database_tuples
  FROM picked p
  LEFT JOIN LATERAL (
       SELECT sum(x.tuples) FILTER (WHERE e->>'schemaname' = $3 AND e->>'relname' = $4)
                AS table_tuples,
              sum(x.tuples) AS database_tuples
         FROM sage.snapshots t
        CROSS JOIN LATERAL (SELECT ` + snapstore.DataSQL("t") + ` AS doc) d
        CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(d.doc) = 'array'
                THEN d.doc ELSE '[]'::jsonb END) AS e
        CROSS JOIN LATERAL (SELECT COALESCE((e->>'n_tup_ins')::int8, 0)
                + COALESCE((e->>'n_tup_upd')::int8, 0)
                + COALESCE((e->>'n_tup_del')::int8, 0) AS tuples) x
        WHERE {db:t} AND t.category = 'tables' AND t.collected_at = p.collected_at
  ) w ON true
 ORDER BY p.collected_at`

func (s *PostgresObservationSource) writeCycles(
	ctx context.Context, schema, relname string, from, to time.Time,
) ([]writeCycle, error) {
	ctx, cancel := context.WithTimeout(ctx, writeCostTimeout)
	defer cancel()
	rows, err := s.historyStore().Query(ctx, writeCyclesSQL, from, to, schema, relname,
		maxWriteCycles)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cycles []writeCycle
	for rows.Next() {
		var c writeCycle
		if err := rows.Scan(&c.At, &c.LSN, &c.Epoch, &c.TableTuples,
			&c.DatabaseTuples); err != nil {
			return nil, err
		}
		cycles = append(cycles, c)
	}
	return cycles, rows.Err()
}
