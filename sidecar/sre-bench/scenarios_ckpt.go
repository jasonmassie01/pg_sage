package srebench

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Checkpoint storm fault programs. Checkpoints, WAL and max_wal_size are
// cluster-wide, so each takes the cluster fixture lock. The undersized
// program lowers max_wal_size with ALTER SYSTEM and always resets it.

func ckptScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerCheckpoint, Class: class, Gold: gold,
		Program: p}
}

func checkpointScenarios() []Scenario {
	undersized := Gold{Root: "max_wal_size_undersized",
		Contributing: []string{"checkpoint_write_burst"}}
	return []Scenario{
		ckptScenario("ckpt-max-wal-size-undersized", ClassPositive, undersized,
			undersizedMaxWAL()),
		ckptScenario("ckpt-forced-checkpoints", ClassPositive,
			Gold{Root: "forced_checkpoints"}, forcedCheckpoints()),
		ckptScenario("ckpt-max-wal-size-undersized-under-load", ClassNoise, undersized,
			withNoise(undersizedMaxWAL())),
		ckptScenario("ckpt-burst-within-max-wal-size", ClassDecoy,
			Gold{Lookalike: "max_wal_size_undersized"}, burstWithinBudget()),
		ckptScenario("ckpt-quiet", ClassBenign, Gold{}, quietCheckpoints(nil)),
	}
}

// undersizedWAL is the max_wal_size the undersized program sets: two
// 16 MiB segments, so PostgreSQL requests a checkpoint every segment.
const (
	undersizedWAL      = "32MB"
	undersizedWALBytes = 32 << 20
)

// volumeDriven reports whether requests checkpoints over wal bytes of
// WAL came at least a quarter of maxWAL apart, as WAL volume requests
// them (the investigator's own volume rule).
func volumeDriven(wal float64, requests int, maxWAL float64) bool {
	return requests > 0 && wal/float64(requests) >= maxWAL/4
}

// undersizedMaxWAL: max_wal_size at its minimum while 64 MiB of WAL is
// written at each sample interval. Requests that were not volume-driven
// are another session's CHECKPOINT commands: the run is contaminated.
func undersizedMaxWAL() program {
	var before int
	var lsn string
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := e.lockCluster(ctx); err != nil {
				return err
			}
			if err := e.alterSystem(ctx, "max_wal_size", undersizedWAL); err != nil {
				return err
			}
			var err error
			if before, err = e.stableCheckpoints(ctx); err != nil {
				return err
			}
			return e.Pool.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn)
		},
		manifest: func(ctx context.Context, e *Env) error {
			return expectSetting(ctx, e, "max_wal_size", undersizedWAL)
		},
		between: writeWAL(64),
		valid: func(ctx context.Context, e *Env) error {
			after, err := e.requestedCheckpoints(ctx)
			if err != nil {
				return err
			}
			wal, err := walSince(ctx, e, lsn)
			if err == nil && !volumeDriven(wal, after-before, undersizedWALBytes) {
				err = &Contaminated{Reason: fmt.Sprintf("%d checkpoints were requested "+
					"over %.0f bytes of WAL: not by WAL volume alone", after-before, wal)}
			}
			return err
		},
		recover: func(ctx context.Context, e *Env) error {
			if err := e.resetSystem(ctx, "max_wal_size"); err != nil {
				return err
			}
			return truncateWAL(ctx, e)
		},
	}
}

func expectSetting(ctx context.Context, e *Env, name, value string) error {
	n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_settings
		WHERE name = $1 AND pg_size_bytes(current_setting($1)) = pg_size_bytes($2)`,
		name, value)
	if err == nil && n != 1 {
		err = fmt.Errorf("%s is not %s", name, value)
	}
	return err
}

// forcedCheckpointsSQL issues CHECKPOINT twice a second for five minutes,
// as a misconfigured backup script would.
const forcedCheckpointsSQL = `DO $$ BEGIN
	FOR i IN 1..600 LOOP
		CHECKPOINT;
		PERFORM pg_sleep(0.5);
	END LOOP;
END $$`

// forcedCheckpoints: a session issuing CHECKPOINT with almost no writes.
func forcedCheckpoints() program {
	var before int
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := e.lockCluster(ctx); err != nil {
				return err
			}
			if _, err := e.Pool.Exec(ctx, "CHECKPOINT"); err != nil {
				return &Unsupported{Reason: "CHECKPOINT not permitted: " + err.Error()}
			}
			var err error
			if before, err = e.stableCheckpoints(ctx); err != nil {
				return err
			}
			_, err = e.background(ctx, "bench_backup_script", forcedCheckpointsSQL)
			return err
		},
		manifest: func(ctx context.Context, e *Env) error {
			return waitFor(ctx, "forced checkpoints", func() (bool, error) {
				n, err := e.requestedCheckpoints(ctx)
				return n >= before+2, err
			})
		},
		valid: func(ctx context.Context, e *Env) error {
			return expectActive(ctx, e, "bench_backup_script", 1)
		},
		recover: gone("bench_backup_script"),
	}
}

// gone verifies that no session of app is left.
func gone(app string) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		return waitFor(ctx, app+" sessions to end", func() (bool, error) {
			n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
				WHERE datname = current_database() AND application_name = $1`, app)
			return n == 0, err
		})
	}
}

func expectActive(ctx context.Context, e *Env, app string, want int) error {
	n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND application_name = $1`, app)
	if err == nil && n < want {
		err = fmt.Errorf("%d %s sessions alive, want %d", n, app, want)
	}
	return err
}

// burstWithinBudget (decoy of an undersized max_wal_size): a fresh
// checkpoint, then 48 MiB of WAL at each sample interval, far inside the
// server's max_wal_size; no checkpoint is requested.
func burstWithinBudget() program {
	return quietCheckpoints(writeWAL(48))
}

// minDecoyMaxWAL is the max_wal_size a burst decoy needs: its 96 MiB
// must not reach the checkpoint distance.
const minDecoyMaxWAL = 512 << 20

// quietCheckpoints runs between at each sample interval after a fresh
// checkpoint; a requested checkpoint during the run is another session's
// (the run is contaminated).
func quietCheckpoints(between func(context.Context, *Env) error) program {
	var base int
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := e.lockCluster(ctx); err != nil {
				return err
			}
			n, err := e.count(ctx, "SELECT pg_size_bytes(current_setting('max_wal_size'))::int")
			if err != nil || n < minDecoyMaxWAL {
				return &Unsupported{Reason: fmt.Sprintf("max_wal_size %d bytes is under "+
					"512 MiB (%v)", n, err)}
			}
			_, err = e.Pool.Exec(ctx, "CHECKPOINT")
			return err
		},
		manifest: func(ctx context.Context, e *Env) error {
			var err error
			base, err = e.stableCheckpoints(ctx)
			return err
		},
		between: between,
		valid: func(ctx context.Context, e *Env) error {
			n, err := e.requestedCheckpoints(ctx)
			if err == nil && n != base {
				err = &Contaminated{Reason: "a checkpoint was requested during a quiet run"}
			}
			return err
		},
		recover: func(ctx context.Context, e *Env) error { return truncateWAL(ctx, e) },
	}
}
