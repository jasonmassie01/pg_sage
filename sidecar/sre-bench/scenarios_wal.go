package srebench

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// WAL retention fault programs. WAL volume, slots and the archiver are
// cluster-wide, so each takes the cluster fixture lock.

// archiveFlag is the file the fixture's archive_command fails on when it
// is non-empty (archive_command = 'test ! -s /tmp/pg_sage_archive_fail').
const archiveFlag = "/tmp/pg_sage_archive_fail"

func lockCluster(ctx context.Context, dsn string) (func(), error) {
	return testdb.LockCluster(ctx, dsn, "wal")
}

func walScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerWAL, Class: class, Gold: gold, Program: p}
}

func walScenarios() []Scenario {
	surge := []string{"write_surge"}
	return []Scenario{
		walScenario("wal-inactive-slot", ClassPositive,
			Gold{Root: "inactive_slot", Contributing: surge}, slotProgram(false)),
		walScenario("wal-slow-consumer", ClassPositive,
			Gold{Root: "slow_consumer", Contributing: surge}, slotProgram(true)),
		walScenario("wal-write-surge", ClassPositive, Gold{Root: "write_surge"},
			walProgram(nil, writeWAL(64))),
		walScenario("wal-archiver-failure", ClassPositive,
			Gold{Root: "archiver_failure"}, archiverFailure()),
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

// steadyWAL writes nothing. WAL is cluster-wide, and other sessions (test
// packages running beside the benchmark) do not take the cluster lock:
// the program waits for a quiet window before the investigation, and
// measures the window it ran in; a run in which the cluster wrote WAL at
// a surge-like rate is contaminated and repeated.
func steadyWAL() program {
	var start string
	var at time.Time
	p := walProgram(func(ctx context.Context, e *Env) error {
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
func writeWAL(mb int) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		if _, err := e.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS bench_wal
			(id int, pad text)`); err != nil {
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

// slotProgram: a physical slot that reserves WAL, with (active) or
// without a consumer, while 48 MiB of WAL is written.
func slotProgram(active bool) program {
	slot := fmt.Sprintf("bench_slot_%d", time.Now().UnixNano())
	var stop func()
	p := walProgram(func(ctx context.Context, e *Env) error {
		if _, err := e.Pool.Exec(ctx,
			"SELECT pg_create_physical_replication_slot($1, true)", slot); err != nil {
			return err
		}
		if !active {
			return nil
		}
		var err error
		stop, err = startPhysicalConsumer(ctx, e, slot)
		return err
	}, writeWAL(48))
	p.manifest = func(ctx context.Context, e *Env) error {
		return waitFor(ctx, "slot state", func() (bool, error) {
			n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_replication_slots
				WHERE slot_name = $1 AND active = $2`, slot, active)
			return n == 1, err
		})
	}
	walRecover := p.recover
	p.recover = func(ctx context.Context, e *Env) error {
		if stop != nil {
			stop()
		}
		if err := dropSlot(ctx, e, slot); err != nil {
			return err
		}
		return walRecover(ctx, e)
	}
	return p
}

func dropSlot(ctx context.Context, e *Env, slot string) error {
	return waitFor(ctx, "slot dropped", func() (bool, error) {
		_, _ = e.Pool.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name)
			FROM pg_catalog.pg_replication_slots WHERE slot_name = $1 AND NOT active`, slot)
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_replication_slots
			WHERE slot_name = $1`, slot)
		return n == 0, err
	})
}

// archiverFailure: archive_command fails while WAL segments complete.
func archiverFailure() program {
	p := walProgram(func(ctx context.Context, e *Env) error {
		if err := archiveFixture(ctx, e); err != nil {
			return err
		}
		if _, err := e.Pool.Exec(ctx, "COPY (SELECT 'fail') TO '"+archiveFlag+"'"); err != nil {
			return err
		}
		return switchAndWait(ctx, e, "failed_count")
	}, func(ctx context.Context, e *Env) error { return switchAndWait(ctx, e, "failed_count") })
	walRecover := p.recover
	p.recover = func(ctx context.Context, e *Env) error {
		if _, err := e.Pool.Exec(ctx,
			"COPY (SELECT 1 WHERE false) TO '"+archiveFlag+"'"); err != nil {
			return err
		}
		if err := recoverArchiver(ctx, e); err != nil {
			return err
		}
		return walRecover(ctx, e)
	}
	p.manifest = func(ctx context.Context, e *Env) error {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_archiver
			WHERE last_failed_time > COALESCE(last_archived_time, '-infinity')`)
		if err == nil && n != 1 {
			err = fmt.Errorf("the archiver is not failing")
		}
		return err
	}
	return p
}

// archiveFixture requires archiving on with the fixture's command.
func archiveFixture(ctx context.Context, e *Env) error {
	var mode, command string
	if err := e.Pool.QueryRow(ctx, `SELECT current_setting('archive_mode'),
		current_setting('archive_command')`).Scan(&mode, &command); err != nil {
		return err
	}
	if mode != "on" || !strings.Contains(command, archiveFlag) {
		return &Unsupported{Reason: "archive fixture not configured (archive_mode " +
			mode + ")"}
	}
	return nil
}

// switchAndWait completes a WAL segment and waits for the archiver
// counter to move.
func switchAndWait(ctx context.Context, e *Env, counter string) error {
	before, err := e.count(ctx, "SELECT "+counter+"::int FROM pg_catalog.pg_stat_archiver")
	if err != nil {
		return err
	}
	if err := writeWAL(1)(ctx, e); err != nil {
		return err
	}
	if _, err := e.Pool.Exec(ctx, "SELECT pg_switch_wal()"); err != nil {
		return err
	}
	return waitFor(ctx, counter+" to move", func() (bool, error) {
		n, err := e.count(ctx, "SELECT "+counter+"::int FROM pg_catalog.pg_stat_archiver")
		return n > before, err
	})
}

// recoverArchiver waits until an archive attempt succeeds after the last
// failure; the archiver may back off for up to a minute.
func recoverArchiver(ctx context.Context, e *Env) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if err := switchAndWait(ctx, e, "archived_count"); err == nil {
			n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_archiver
				WHERE last_archived_time > COALESCE(last_failed_time, '-infinity')`)
			if err == nil && n == 1 {
				return nil
			}
		}
	}
	return fmt.Errorf("the archiver did not recover in 90 s")
}
