package querystore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// The hourly per-queryid rollup (evid-01a). The richest per-query evidence
// is the snapshot queries category (calls, time, blocks, temp, WAL of the
// admitted queryids), kept 90 days; the query store keeps 14. The rollup
// sums, per queryid and UTC hour, the differences between consecutive
// queries snapshots of one statistics epoch (R10, EpochBreak), so a reset
// or a suspend/resume undercounts one interval and never goes negative.

// ErrRollupHour marks an hour the rollup refuses: zero, or in the future.
var ErrRollupHour = errors.New("query rollup: invalid hour")

// rollupStatementTimeout bounds every statement of one hour's rollup.
var rollupStatementTimeout = 30 * time.Second

// rollupLockKey is the first key of the rollup's transaction advisory
// lock; the second is the database id (0 in monitored mode).
const rollupLockKey = "sage.query_rollup"

// SnapshotInterval is one pair of consecutive queries snapshots of a
// queryid: its deltas, all zero when the pair crosses an epoch (EpochBreak
// set), between From and To.
type SnapshotInterval struct {
	QueryID                      int64
	From, To                     time.Time
	EpochBreak                   bool
	Calls                        int64
	ExecMs                       float64
	Rows, SharedBlksHit          int64
	SharedBlksRead, TempBlksRead int64
	TempBlksWritten, WALBytes    int64
}

// SnapshotIntervals returns the snapshot intervals of queryids (all when
// empty) whose later snapshot is in [from, to), read in db's history store
// (histstore.Resolve), ordered by queryid and time. Pairs more than
// AnchorLookback apart are not intervals: the query left the admitted set.
func SnapshotIntervals(ctx context.Context, db any, from, to time.Time,
	queryids []int64) ([]SnapshotInterval, error) {
	if queryids == nil {
		queryids = []int64{}
	}
	rows, err := histstore.Resolve(db).Query(ctx, snapshotIntervalsSQL, from, to,
		AnchorLookback, queryids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SnapshotInterval, error) {
		var iv SnapshotInterval
		err := r.Scan(&iv.QueryID, &iv.From, &iv.To, &iv.EpochBreak, &iv.Calls, &iv.ExecMs,
			&iv.Rows, &iv.SharedBlksHit, &iv.SharedBlksRead, &iv.TempBlksRead,
			&iv.TempBlksWritten, &iv.WALBytes)
		return iv, err
	})
}

// HourReport is what one hour's rollup wrote.
type HourReport struct {
	Hour          time.Time
	Snapshots     int // queries snapshots in the hour
	Queryids      int // hourly rows written
	EpochBreaks   int // rows with an epoch break
	SampledExecMs float64
	// DBExecMs is the database's own statement time in the hour (all
	// entries but pg_sage's); nil when unknown.
	DBExecMs    *float64
	EpochSource string // "recorded" or "counters_only"
}

// Empty reports an hour without a queries snapshot.
func (r HourReport) Empty() bool { return r.Snapshots == 0 }

// rollupRow is one queryid's sum over an hour's intervals.
type rollupRow struct {
	queryID                int64
	calls, rows, hit, read int64
	tread, twrite, wal     int64
	execMs                 float64
	epochBreak             bool
	pairs                  int
}

// sumIntervals sums intervals per queryid, ordered by queryid: a break
// adds nothing and flags the row; pairs counts the same-epoch intervals.
func sumIntervals(ivs []SnapshotInterval) []rollupRow {
	byID := map[int64]*rollupRow{}
	for _, iv := range ivs {
		r := byID[iv.QueryID]
		if r == nil {
			r = &rollupRow{queryID: iv.QueryID}
			byID[iv.QueryID] = r
		}
		if iv.EpochBreak {
			r.epochBreak = true
			continue
		}
		r.pairs++
		r.calls += iv.Calls
		r.execMs += iv.ExecMs
		r.rows += iv.Rows
		r.hit += iv.SharedBlksHit
		r.read += iv.SharedBlksRead
		r.tread += iv.TempBlksRead
		r.twrite += iv.TempBlksWritten
		r.wal += iv.WALBytes
	}
	out := make([]rollupRow, 0, len(byID))
	for _, r := range byID {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b rollupRow) int { return cmp.Compare(a.queryID, b.queryID) })
	return out
}

// checkHour truncates hour to its UTC hour and refuses a zero or future one.
func checkHour(hour time.Time) (time.Time, error) {
	if hour.IsZero() {
		return time.Time{}, fmt.Errorf("%w: zero hour", ErrRollupHour)
	}
	h := hour.UTC().Truncate(time.Hour)
	if h.After(time.Now()) {
		return time.Time{}, fmt.Errorf("%w: hour %s is in the future", ErrRollupHour,
			h.Format(time.RFC3339))
	}
	return h, nil
}

// rollupErr wraps a failed step of hour's rollup, naming the statement
// bound when it was hit.
func rollupErr(hour time.Time, step string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return fmt.Errorf("query rollup %s: %s: statement_timeout %s exceeded: %w",
			hour.Format(time.RFC3339), step, rollupStatementTimeout, err)
	}
	return fmt.Errorf("query rollup %s: %s: %w", hour.Format(time.RFC3339), step, err)
}

// RollupHour rolls up hour (truncated to its UTC hour) of db's history
// store (histstore.Resolve) in one transaction, under a per-database
// advisory lock and a statement bound, and upserts its rows and coverage:
// rolling an hour again gives the same rows. It writes only sage history.
func RollupHour(ctx context.Context, db any, hour time.Time) (HourReport, error) {
	h, err := checkHour(hour)
	if err != nil {
		return HourReport{}, err
	}
	st := histstore.Resolve(db)
	tx, err := st.Begin(ctx)
	if err != nil {
		return HourReport{}, rollupErr(h, "begin", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	txs := st.WithDB(tx)
	bound := fmt.Sprintf("SET LOCAL statement_timeout = %d",
		rollupStatementTimeout.Milliseconds())
	if _, err := tx.Exec(ctx, bound); err != nil {
		return HourReport{}, rollupErr(h, "set the statement bound", err)
	}
	if _, err := tx.Exec(ctx, "/* pg_sage */ SELECT pg_advisory_xact_lock(hashtext($1), $2)",
		rollupLockKey, int32(st.DatabaseID())); err != nil {
		return HourReport{}, rollupErr(h, "take the rollup lock", err)
	}
	rep, err := rollupInTx(ctx, txs, h)
	if err != nil {
		return HourReport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return HourReport{}, rollupErr(h, "commit", err)
	}
	return rep, nil
}

// rollupInTx reads, sums and writes hour h on txs (a transaction-bound
// store).
func rollupInTx(ctx context.Context, txs histstore.Store, h time.Time) (HourReport, error) {
	end := h.Add(time.Hour)
	ivs, err := SnapshotIntervals(ctx, txs, h, end, nil)
	if err != nil {
		return HourReport{}, rollupErr(h, "read sage.snapshots", err)
	}
	rows := sumIntervals(ivs)
	rep := HourReport{Hour: h, Queryids: len(rows), EpochSource: "counters_only"}
	var known bool
	var dealloc *int64
	if err := txs.QueryRow(ctx, hourCoverageSQL, h, end, AnchorLookback).Scan(
		&rep.Snapshots, &known, &rep.DBExecMs, &dealloc); err != nil {
		return HourReport{}, rollupErr(h, "read the coverage of sage.snapshots", err)
	}
	if known {
		rep.EpochSource = "recorded"
	}
	if err := writeHourRows(ctx, txs, h, rows); err != nil {
		return HourReport{}, rollupErr(h, "write sage.query_rollup_hourly", err)
	}
	for _, r := range rows {
		rep.SampledExecMs += r.execMs
		if r.epochBreak {
			rep.EpochBreaks++
		}
	}
	_, err = txs.Exec(ctx, upsertCoverageSQL, h, rep.DBExecMs, rep.SampledExecMs,
		rep.Queryids, rep.EpochBreaks, dealloc, min(rep.Snapshots, math.MaxInt16),
		rep.EpochSource)
	if err != nil {
		return HourReport{}, rollupErr(h, "write sage.query_rollup_coverage", err)
	}
	return rep, nil
}

// writeHourRows upserts hour h's rows and drops the rows of queryids the
// hour no longer has.
func writeHourRows(ctx context.Context, txs histstore.Store, h time.Time,
	rows []rollupRow) error {
	n := len(rows)
	ids, calls, rws := make([]int64, n), make([]int64, n), make([]int64, n)
	hit, read, tread := make([]int64, n), make([]int64, n), make([]int64, n)
	twrite, wal, execMs := make([]int64, n), make([]int64, n), make([]float64, n)
	brk, pairs := make([]bool, n), make([]int16, n)
	for i, r := range rows {
		ids[i], calls[i], rws[i], hit[i], read[i] = r.queryID, r.calls, r.rows, r.hit, r.read
		tread[i], twrite[i], wal[i], execMs[i] = r.tread, r.twrite, r.wal, r.execMs
		brk[i], pairs[i] = r.epochBreak, int16(min(r.pairs, math.MaxInt16))
	}
	if _, err := txs.Exec(ctx, dropStaleHourlySQL, h, ids); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err := txs.Exec(ctx, upsertHourlySQL, h, ids, calls, execMs, rws, hit, read, tread,
		twrite, wal, brk, pairs)
	return err
}
