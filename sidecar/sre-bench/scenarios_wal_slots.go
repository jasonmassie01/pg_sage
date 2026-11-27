package srebench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Replication slot and archiver fault programs and their fixtures. Like
// the WAL programs in scenarios_wal.go, each takes the cluster fixture
// lock through walProgram.

// archiveFlag is the file the fixture's archive_command fails on when it
// is non-empty (archive_command = 'test ! -s /tmp/pg_sage_archive_fail').
const archiveFlag = "/tmp/pg_sage_archive_fail"

// slotProgram: a slot that reserves WAL while 48 MiB of WAL is written.
// Inactive, it is a physical slot without a consumer; active, it is a
// logical slot whose consumer never confirms (a slow consumer).
func slotProgram(active bool) program {
	slot := uniqueName(slotPrefix)
	var c *consumer
	p := walProgram(func(ctx context.Context, e *Env) error {
		if !active {
			_, err := e.Pool.Exec(ctx,
				"SELECT pg_create_physical_replication_slot($1, true)", slot)
			return err
		}
		if err := createLogicalSlot(ctx, e, slot); err != nil {
			return err
		}
		var err error
		c, err = startLogicalConsumer(ctx, e, slot, false)
		return err
	}, writeWAL(48))
	p.manifest = slotActive(slot, active)
	p.valid = func(context.Context, *Env) error { return consumerAlive(c) }
	p.recover = recoverSlot(&c, slot, p.recover)
	return p
}

// keepingUpSlot (decoy of a slow consumer): a logical slot whose
// consumer confirms what it receives, on a quiet cluster; its retention
// stays under 1 MiB.
func keepingUpSlot() program {
	slot := uniqueName(slotPrefix)
	var c *consumer
	p := quietWAL(func(ctx context.Context, e *Env) error {
		if err := createLogicalSlot(ctx, e, slot); err != nil {
			return err
		}
		var err error
		c, err = startLogicalConsumer(ctx, e, slot, true)
		return err
	})
	p.manifest = func(ctx context.Context, e *Env) error {
		if err := slotActive(slot, true)(ctx, e); err != nil {
			return err
		}
		return keepsUp(ctx, e, slot)
	}
	quiet := p.valid
	p.valid = func(ctx context.Context, e *Env) error {
		if err := consumerAlive(c); err != nil {
			return err
		}
		return quiet(ctx, e)
	}
	p.recover = recoverSlot(&c, slot, p.recover)
	return p
}

func slotActive(slot string, active bool) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		return waitFor(ctx, "slot state", func() (bool, error) {
			n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_replication_slots
				WHERE slot_name = $1 AND active = $2`, slot, active)
			return n == 1, err
		})
	}
}

// runningXactsInterval: the background writer logs a running-xacts record
// at most this often (LOG_SNAPSHOT_INTERVAL_MS), and a logical slot's
// restart_lsn only moves at one. slotConfirmWait outlasts two of them, so
// a keeping-up slot is not judged before its retention can shrink.
const (
	runningXactsInterval = 15 * time.Second
	slotConfirmWait      = 2*runningXactsInterval + 15*time.Second
)

// keepsUp writes a little WAL in this database and waits until the
// slot's consumer confirms past where it was, with under 1 MiB retained.
func keepsUp(ctx context.Context, e *Env, slot string) error {
	var before string
	if err := e.Pool.QueryRow(ctx, `SELECT confirmed_flush_lsn::text
		FROM pg_catalog.pg_replication_slots WHERE slot_name = $1`, slot).
		Scan(&before); err != nil {
		return err
	}
	if _, err := e.Pool.Exec(ctx, createWALTableSQL+`;
		INSERT INTO bench_wal SELECT g, 'x' FROM generate_series(1, 100) g`); err != nil {
		return err
	}
	return waitForWithin(ctx, "slot consumer to confirm", slotConfirmWait, func() (bool,
		error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_replication_slots
			WHERE slot_name = $1 AND confirmed_flush_lsn > $2::pg_lsn
			  AND pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) < $3`,
			slot, before, 1<<20)
		return n == 1, err
	})
}

func consumerAlive(c *consumer) error {
	if c == nil {
		return nil
	}
	return c.alive()
}

// recoverSlot stops the consumer, drops the slot, then runs next.
func recoverSlot(c **consumer, slot string, next func(context.Context, *Env) error) func(
	context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		if *c != nil {
			(*c).stop()
			*c = nil
		}
		if err := dropSlot(ctx, e, slot); err != nil {
			return err
		}
		return step(next, ctx, e)
	}
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
		err := archiveFixture(ctx, e)
		var u *Unsupported
		if errors.As(err, &u) {
			// Skipped: nothing was injected, and the archiver is off.
			return walRecover(ctx, e)
		}
		if err != nil {
			return err
		}
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
