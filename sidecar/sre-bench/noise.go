package srebench

import (
	"context"
	"fmt"
)

// Background noise (CHECK-42): unrelated load running under a clean
// fault. Idle connections of another application, long active
// statements of a reporting application and a slow writer committing
// small rows to its own table. The noise takes no lock that a fault
// session waits on, and it writes WAL far below the surge floor.
const (
	noiseIdle   = 6
	noiseActive = 2
	noiseApp    = "bench_noise_idle"
)

// noiseWriterSQL commits one row every 50 ms for up to two minutes; a
// DO block sent alone may commit between rows.
const noiseWriterSQL = `DO $$ BEGIN
	FOR i IN 1..2400 LOOP
		INSERT INTO %s VALUES (i + 1, i);
		COMMIT;
		PERFORM pg_sleep(0.05);
	END LOOP;
END $$`

// withNoise runs p under background noise started before the fault.
func withNoise(p program) program {
	tb := newTable("noise")
	noisy := p
	noisy.inject = func(ctx context.Context, e *Env) error {
		if err := tb.create(ctx, e); err != nil {
			return err
		}
		if err := e.idle(ctx, noiseApp, noiseIdle); err != nil {
			return err
		}
		for i := 0; i < noiseActive; i++ {
			if _, err := e.background(ctx, "bench_noise_report",
				"SELECT pg_sleep(120)"); err != nil {
				return err
			}
		}
		if _, err := e.background(ctx, "bench_noise_writer",
			fmt.Sprintf(noiseWriterSQL, tb.name)); err != nil {
			return err
		}
		return step(p.inject, ctx, e)
	}
	noisy.manifest = func(ctx context.Context, e *Env) error {
		if err := noiseRunning(ctx, e); err != nil {
			return err
		}
		return step(p.manifest, ctx, e)
	}
	noisy.recover = func(ctx context.Context, e *Env) error {
		if err := step(p.recover, ctx, e); err != nil {
			return err
		}
		return tb.drop(ctx, e)
	}
	return noisy
}

// noiseRunning waits until the noise is present: its idle pool, the
// reporting statements and the writer, which must still be writing.
func noiseRunning(ctx context.Context, e *Env) error {
	if err := expectIdle(noiseApp, noiseIdle)(ctx, e); err != nil {
		return err
	}
	return waitFor(ctx, "noise statements running", func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE datname = current_database() AND state = 'active'
			  AND application_name IN ('bench_noise_report', 'bench_noise_writer')`)
		return n == noiseActive+1, err
	})
}
