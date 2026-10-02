package srebench

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre"
)

// LWLock contention fault programs: a commit storm (many sessions
// committing one-row inserts queue on the WAL write lock) and a
// partition fan-out (queries locking more relations than the fast-path
// slots hold queue on the lock manager). LWLocks are cluster-wide: the
// decoy and benign runs watch other databases' LWLock waits and are
// repeated when other load was there.

func lwlockScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerLWLock, Class: class, Gold: gold, Program: p}
}

func lwlockScenarios() []Scenario {
	walWrite := Gold{Root: "wal_write_contention"}
	return []Scenario{
		lwlockScenario("lwlock-commit-storm", ClassPositive, walWrite, commitStorm(24)),
		lwlockScenario("lwlock-partition-fan-out", ClassPositive,
			Gold{Root: "lock_manager_contention"}, partitionFanOut(100, 32)),
		lwlockScenario("lwlock-commit-storm-under-load", ClassNoise, walWrite,
			withNoise(commitStorm(24))),
		lwlockScenario("lwlock-light-commits", ClassDecoy,
			Gold{Lookalike: "wal_write_contention"}, lightCommits()),
		lwlockScenario("lwlock-idle", ClassBenign, Gold{}, watchedQuiet(nil)),
	}
}

// lwlockWaiters is the most backends of this database waiting on one of
// events in one sample.
func lwlockWaiters(ctx context.Context, e *Env, events ...string) (int, error) {
	return e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'LWLock'
		  AND wait_event = ANY($1)`, events)
}

// expectWaiters polls until at least n backends wait on events at once.
func expectWaiters(n int, events ...string) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		return waitLong(ctx, fmt.Sprintf("%d backends waiting on %v", n, events),
			func() (bool, error) {
				got, err := lwlockWaiters(ctx, e, events...)
				return got >= n, err
			})
	}
}

// commitTable is a table one-row inserts commit into.
func commitTable(ctx context.Context, e *Env) (string, error) {
	name := pgx.Identifier{uniqueName("bench_commits_")}.Sanitize()
	_, err := e.Pool.Exec(ctx, "CREATE TABLE "+name+" (id bigserial, v int)")
	return name, err
}

func dropTable(name *string) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		if *name == "" {
			return nil
		}
		_, err := e.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+*name)
		return err
	}
}

// serverLoopSQL repeats body, committing after each round, for at most
// two minutes: the load stays on the server (a client round trip per
// statement would leave most sessions idle), and it ends on its own if
// the bench dies.
const serverLoopSQL = `DO $$ DECLARE
	stop timestamptz := clock_timestamp() + interval '120 seconds';
	n int;
BEGIN
	WHILE clock_timestamp() < stop LOOP
		%s;
		COMMIT;%s
	END LOOP;
END $$`

// serverLoop starts n sessions running body in a server-side loop,
// sleeping pause seconds (a numeric literal, or "") after each commit.
func serverLoop(ctx context.Context, e *Env, app, body string, n int,
	pause string) error {
	wait := ""
	if pause != "" {
		wait = " PERFORM pg_sleep(" + pause + ");"
	}
	for i := 0; i < n; i++ {
		if _, err := e.background(ctx, app, fmt.Sprintf(serverLoopSQL, body, wait)); err != nil {
			return err
		}
	}
	return nil
}

// commitStorm: n sessions commit one-row inserts as fast as they can.
func commitStorm(n int) program {
	const app = "bench_writer"
	var table string
	return program{
		inject: func(ctx context.Context, e *Env) error {
			var err error
			if table, err = commitTable(ctx, e); err != nil {
				return err
			}
			return serverLoop(ctx, e, app, "INSERT INTO "+table+" (v) VALUES (1)", n, "")
		},
		manifest: expectWaiters(8, "WALWrite", "WALInsert"),
		valid:    func(ctx context.Context, e *Env) error { return expectActive(ctx, e, app, n) },
		recover:  dropTable(&table),
	}
}

// partitionDDL creates a hash-partitioned table with an index, so a
// query without pruning locks two relations per partition. %[1]s is the
// table name as a SQL literal (a generated identifier), %[2]d the
// partition count.
const partitionDDL = `DO $$ BEGIN
	EXECUTE format('CREATE TABLE %%I (id int, v int) PARTITION BY HASH (id)', %[1]s);
	FOR i IN 0..%[2]d - 1 LOOP
		EXECUTE format('CREATE TABLE %%I PARTITION OF %%I FOR VALUES WITH '
			'(MODULUS %[2]d, REMAINDER %%s)', %[1]s || '_p' || i, %[1]s, i);
	END LOOP;
	EXECUTE format('CREATE INDEX ON %%I (v)', %[1]s);
END $$`

// partitionFanOut: n sessions query a parts-way partitioned table
// without partition pruning.
func partitionFanOut(parts, n int) program {
	const app = "bench_fan_out"
	var table string
	return program{
		inject: func(ctx context.Context, e *Env) error {
			name := uniqueName("bench_parts_")
			lit := "'" + name + "'"
			if err := e.simple(ctx, fmt.Sprintf(partitionDDL, lit, parts)); err != nil {
				return err
			}
			table = pgx.Identifier{name}.Sanitize()
			return serverLoop(ctx, e, app, "EXECUTE 'SELECT count(*) FROM "+table+
				" WHERE v = 1' INTO n", n, "")
		},
		manifest: expectWaiters(8, "LockManager", "lock_manager"),
		valid:    func(ctx context.Context, e *Env) error { return expectActive(ctx, e, app, n) },
		recover:  dropTable(&table),
	}
}

// lightCommits (decoy of WAL write contention): two sessions commit a
// row every 50 ms; an occasional WAL write wait is not contention.
func lightCommits() program {
	const app = "bench_light_writer"
	var table string
	p := watchedQuiet(func(ctx context.Context, e *Env) error {
		var err error
		if table, err = commitTable(ctx, e); err != nil {
			return err
		}
		return serverLoop(ctx, e, app, "INSERT INTO "+table+" (v) VALUES (1)", 2, "0.05")
	})
	quiet := p.valid
	p.valid = func(ctx context.Context, e *Env) error {
		if err := expectActive(ctx, e, app, 2); err != nil {
			return err
		}
		return quiet(ctx, e)
	}
	next := p.recover
	p.recover = func(ctx context.Context, e *Env) error {
		if err := next(ctx, e); err != nil {
			return err
		}
		return dropTable(&table)(ctx, e)
	}
	return p
}

// watchedQuiet runs setup, then watches other databases' LWLock waits
// from the manifestation to the end of the investigation.
func watchedQuiet(setup func(context.Context, *Env) error) program {
	var s *sampler
	return program{
		inject: setup,
		manifest: func(ctx context.Context, e *Env) error {
			s = e.startSampler()
			return nil
		},
		valid: func(context.Context, *Env) error {
			err := s.stop()
			s = nil
			return err
		},
		recover: func(context.Context, *Env) error {
			_ = s.stop() // a run that ended early is not scored
			s = nil
			return nil
		},
	}
}
