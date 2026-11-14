package srebench

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/clusterlock"
	"github.com/pg-sage/sidecar/internal/sre"
)

// WAL retention fault programs. WAL volume, slots and the archiver are
// cluster-wide, so each takes the cluster fixture lock.

func lockCluster(ctx context.Context, dsn string) (func(), error) {
	return clusterlock.Lock(ctx, dsn, "wal")
}

func walScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerWAL, Class: class, Gold: gold, Program: p}
}

func walScenarios() []Scenario {
	surge := []string{"write_surge"}
	inactive := Gold{Root: "inactive_slot", Contributing: surge}
	return []Scenario{
		walScenario("wal-inactive-slot", ClassPositive, inactive, slotProgram(false)),
		walScenario("wal-slow-consumer", ClassPositive,
			Gold{Root: "slow_consumer", Contributing: surge}, slotProgram(true)),
		walScenario("wal-write-surge", ClassPositive, Gold{Root: "write_surge"},
			surgeProgram(64)),
		walScenario("wal-archiver-failure", ClassPositive,
			Gold{Root: "archiver_failure"}, archiverFailure()),
		walScenario("wal-inactive-slot-under-load", ClassNoise, inactive,
			withNoise(slotProgram(false))),
		walScenario("wal-write-surge-under-load", ClassNoise, Gold{Root: "write_surge"},
			withNoise(surgeProgram(64))),
		walScenario("wal-slot-keeping-up", ClassDecoy, Gold{Lookalike: "slow_consumer"},
			keepingUpSlot()),
		walScenario("wal-steady", ClassBenign, Gold{}, steadyWAL()),
	}
}

// walProgram takes the cluster lock, requires a clean cluster (no slots,
// archiving healthy) and runs between at the sample interval.
func walProgram(inject, between func(context.Context, *Env) error) program {
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := e.lockCluster(ctx); err != nil {
				return err
			}
			if err := cleanCluster(ctx, e); err != nil {
				return err
			}
			return step(inject, ctx, e)
		},
		between: between,
		recover: func(ctx context.Context, e *Env) error { return truncateWAL(ctx, e) },
	}
}

// cleanCluster refuses to run a WAL scenario on a cluster that already
// has slots or a failing archiver: the scenario would not be the cause.
func cleanCluster(ctx context.Context, e *Env) error {
	slots, err := e.count(ctx, "SELECT count(*) FROM pg_catalog.pg_replication_slots")
	if err != nil {
		return err
	}
	failing, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_archiver
		WHERE last_failed_time > COALESCE(last_archived_time, '-infinity')`)
	if err != nil {
		return err
	}
	if slots+failing > 0 {
		return fmt.Errorf("cluster not clean: %d slots, archiver failing=%v", slots,
			failing > 0)
	}
	return nil
}

// quietWALRate is the cluster WAL rate a steady window must stay under:
// half the investigator's 4 MiB/s surge floor, so a burst inside the
// probes' shorter sample window is not diluted below the floor here.
const quietWALRate = 2 << 20

// Surge sizing. The investigator calls WAL volume a surge when its rate is
// at least 4 MiB/s and 3 times the cluster's average since the statistics
// reset. On a cluster that already wrote a lot (CI runs the bench after
// the full suites on the same server) a fixed surge can fall under that
// ratio, so the surge is sized from the measured average with a margin.
const (
	surgeFloorRate = 4 << 20 // the investigator's surge floor, bytes/s
	surgeMargin    = 4       // times the long-run average (the investigator needs 3)
	maxSurgeMiB    = 512     // larger surges are not attempted
	// surgeWindow is the sample window a surge's rate is computed over,
	// allowing the write itself to stretch the 3 s sample interval.
	surgeWindow = 2 * sampleInterval
)

// surgeMiB is the surge size, in MiB, that clears the floor and is
// surgeMargin times avg (bytes/s; negative or zero when unknown or idle)
// over surgeWindow, and at least minMiB.
func surgeMiB(minMiB int, avg float64) (int, error) {
	rate := math.Max(surgeFloorRate, surgeMargin*math.Max(avg, 0))
	need := int(math.Ceil(rate*surgeWindow.Seconds()/(1<<20) - 1e-9))
	mb := max(minMiB, need)
	if mb > maxSurgeMiB {
		return 0, &Unsupported{Reason: fmt.Sprintf("the cluster's long-run WAL rate "+
			"(%.0f bytes/s) needs a %d MiB surge, over the %d MiB limit", avg, mb,
			maxSurgeMiB)}
	}
	return mb, nil
}

// walAverage is the cluster's WAL rate since the statistics reset, the
// average the investigator compares a surge with.
func walAverage(ctx context.Context, e *Env) (float64, error) {
	var avg float64
	err := e.Pool.QueryRow(ctx, `SELECT wal_bytes::float8 /
		GREATEST(EXTRACT(EPOCH FROM clock_timestamp() - stats_reset), 1)
		FROM pg_catalog.pg_stat_wal`).Scan(&avg)
	if err != nil {
		return 0, fmt.Errorf("read the cluster WAL average: %w", err)
	}
	return avg, nil
}

// surgeProgram writes a surge (at least minMiB, sized by surgeMiB) between
// the samples on a clean cluster, and checks afterwards that at least half
// of it was written.
func surgeProgram(minMiB int) program {
	var start string
	mb := minMiB
	p := walProgram(func(ctx context.Context, e *Env) error {
		avg, err := walAverage(ctx, e)
		if err != nil {
			return err
		}
		mb, err = surgeMiB(minMiB, avg)
		return err
	}, func(ctx context.Context, e *Env) error {
		if err := e.Pool.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").
			Scan(&start); err != nil {
			return err
		}
		return writeWAL(mb)(ctx, e)
	})
	p.manifest = cleanCluster
	p.valid = func(ctx context.Context, e *Env) error {
		bytes, err := walSince(ctx, e, start)
		if err == nil && bytes < float64(mb<<19) {
			err = fmt.Errorf("the surge wrote %.0f bytes of WAL, want about %d MiB", bytes,
				mb)
		}
		return err
	}
	return p
}

// quietWAL runs setup, then waits for a quiet window before the
// investigation and measures the window it ran in. WAL is cluster-wide,
// and other sessions (test packages running beside the benchmark) do not
// take the cluster lock: a run in which the cluster wrote WAL at a
// surge-like rate is contaminated and repeated.
func quietWAL(setup func(context.Context, *Env) error) program {
	var start string
	var at time.Time
	p := walProgram(func(ctx context.Context, e *Env) error {
		if err := step(setup, ctx, e); err != nil {
			return err
		}
		return awaitQuiet(ctx, quietTries, func(ctx context.Context) (float64,
			time.Duration, error) {
			return measureWAL(ctx, e, quietProbe)
		})
	}, func(ctx context.Context, e *Env) error {
		at = time.Now()
		return e.Pool.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&start)
	})
	p.valid = func(ctx context.Context, e *Env) error {
		bytes, err := walSince(ctx, e, start)
		if err != nil {
			return err
		}
		return quietWindow(bytes, time.Since(at))
	}
	return p
}

// steadyWAL writes nothing, on a cluster without slots or a failing
// archiver.
func steadyWAL() program {
	p := quietWAL(nil)
	p.manifest = cleanCluster
	return p
}

// quietTries windows of quietProbe bound the wait for a quiet cluster.
const (
	quietTries = 45
	quietProbe = 2 * time.Second
)

// awaitQuiet measures up to tries windows and returns at the first quiet
// one; a cluster busy throughout is contaminated.
func awaitQuiet(ctx context.Context, tries int,
	measure func(context.Context) (float64, time.Duration, error)) error {
	var last error
	for i := 0; i < tries; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		bytes, elapsed, err := measure(ctx)
		if err != nil {
			return err
		}
		if last = quietWindow(bytes, elapsed); last == nil {
			return nil
		}
	}
	return last
}

// measureWAL returns the cluster's WAL bytes over a window of d.
func measureWAL(ctx context.Context, e *Env, d time.Duration) (float64, time.Duration,
	error) {
	var lsn string
	if err := e.Pool.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		return 0, 0, err
	}
	at := time.Now()
	if err := sleepRest(ctx, d); err != nil {
		return 0, 0, err
	}
	bytes, err := walSince(ctx, e, lsn)
	return bytes, time.Since(at), err
}

func walSince(ctx context.Context, e *Env, lsn string) (float64, error) {
	var bytes float64
	err := e.Pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(),
		$1::pg_lsn)::float8`, lsn).Scan(&bytes)
	return bytes, err
}

// quietWindow reports a window whose WAL rate reached quietWALRate.
func quietWindow(bytes float64, elapsed time.Duration) error {
	if elapsed <= 0 {
		return fmt.Errorf("steady window was not measured")
	}
	if rate := bytes / elapsed.Seconds(); rate >= quietWALRate {
		return &Contaminated{Reason: fmt.Sprintf(
			"the cluster wrote %.0f bytes/s of WAL during a steady window", rate)}
	}
	return nil
}

// writeWAL writes about mb MiB of WAL.
// createWALTableSQL creates the table the WAL programs write their WAL into.
// Autovacuum is kept off it: vacuuming the fresh surge after a checkpoint
// writes it all again as full-page images when data checksums are on (the
// default from PostgreSQL 18), WAL a consumer has not confirmed yet at the
// next sample (walfixture_db_test.go).
const createWALTableSQL = `CREATE TABLE IF NOT EXISTS bench_wal (id int, pad text)
	WITH (autovacuum_enabled = off)`

func writeWAL(mb int) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		if _, err := e.Pool.Exec(ctx, createWALTableSQL); err != nil {
			return err
		}
		_, err := e.Pool.Exec(ctx, `INSERT INTO bench_wal
			SELECT g, repeat(md5(g::text), 32) FROM generate_series(1, $1) g`, mb*1000)
		return err
	}
}

func truncateWAL(ctx context.Context, e *Env) error {
	_, err := e.Pool.Exec(ctx, "DROP TABLE IF EXISTS bench_wal")
	return err
}
