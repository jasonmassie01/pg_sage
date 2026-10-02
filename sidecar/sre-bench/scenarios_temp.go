package srebench

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Temp-file explosion fault programs. Temp files, pg_stat_database and
// pg_stat_statements are attributed to this database, so they need no
// cluster lock. Sessions spill with a 1 MB work_mem.

func tempScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerTempFiles, Class: class, Gold: gold,
		Program: p}
}

func tempScenarios() []Scenario {
	runaway := Gold{Root: "runaway_spill_query"}
	lookalike := Gold{Lookalike: "runaway_spill_query"}
	return []Scenario{
		tempScenario("temp-runaway-query", ClassPositive, runaway,
			liveSpill(2000000, 64<<20)),
		tempScenario("temp-repeated-spill", ClassPositive,
			Gold{Root: "repeated_spill_statement"}, repeatedSpill()),
		tempScenario("temp-workload-spills", ClassPositive,
			Gold{Root: "work_mem_undersized"}, workloadSpills()),
		tempScenario("temp-runaway-query-under-load", ClassNoise, runaway,
			withNoise(liveSpill(2000000, 64<<20))),
		tempScenario("temp-finished-spill", ClassDecoy, lookalike, finishedSpill()),
		tempScenario("temp-small-live-spill", ClassDecoy, lookalike, liveSpill(150000, 1)),
		tempScenario("temp-none", ClassBenign, Gold{}, program{manifest: noLiveTemp,
			recover: noLiveTemp}),
	}
}

const spillSetup = "SET work_mem = '1MB'"

// sortSQL sorts rows by a text key; with a 1 MB work_mem it spills.
func sortSQL(rows int, key string) string {
	return fmt.Sprintf(`SELECT count(*) FROM (SELECT g FROM generate_series(1, %d) g
		ORDER BY %s) s`, rows, key)
}

// liveSpillSQL sorts rows to temp files, then reads them one row every
// 10 ms: the sort's temp files stay open while the statement runs.
func liveSpillSQL(rows int) string {
	return fmt.Sprintf(`%s; SELECT pg_sleep(0.01) FROM (SELECT g FROM
		generate_series(1, %d) g ORDER BY md5(g::text)) s`, spillSetup, rows)
}

// liveTemp is the live temp bytes of this database's largest holder.
func liveTemp(ctx context.Context, e *Env) (int64, error) {
	var bytes int64
	err := e.Pool.QueryRow(ctx, `SELECT COALESCE(max(f.bytes), 0)::int8 FROM (
		    SELECT (regexp_match(t.name, '^pgsql_tmp([0-9]+)\.'))[1]::int AS pid,
		           sum(t.size) AS bytes
		    FROM pg_catalog.pg_ls_tmpdir() t GROUP BY 1) f
		JOIN pg_catalog.pg_stat_activity a ON a.pid = f.pid
		WHERE a.datname = current_database()`).Scan(&bytes)
	return bytes, err
}

// liveSpill holds a live spill of at least minBytes (the runaway query),
// or a small one (minBytes 1: the decoy stays under the runaway floor).
func liveSpill(rows int, minBytes int64) program {
	const app = "bench_report"
	return program{
		inject: func(ctx context.Context, e *Env) error {
			_, err := e.background(ctx, app, liveSpillSQL(rows))
			return err
		},
		manifest: func(ctx context.Context, e *Env) error {
			return waitLong(ctx, "a live temp spill", func() (bool, error) {
				b, err := liveTemp(ctx, e)
				return b >= minBytes && (minBytes > 1 || b < 48<<20), err
			})
		},
		valid: func(ctx context.Context, e *Env) error { return expectActive(ctx, e, app, 1) },
		recover: func(ctx context.Context, e *Env) error {
			if err := gone(app)(ctx, e); err != nil {
				return err
			}
			return noLiveTemp(ctx, e)
		},
	}
}

// waitLong polls cond for up to a minute (a large sort under load).
func waitLong(ctx context.Context, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(time.Minute)
	for {
		ok, err := cond()
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w", what, err)
		case ok:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("%s: not reached in a minute", what)
		}
		if err := sleepRest(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// pgss requires pg_stat_statements, created here when it can be.
func pgss(ctx context.Context, e *Env) error {
	if _, err := e.Pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		return &Unsupported{Reason: "pg_stat_statements not installable: " + err.Error()}
	}
	if _, err := e.Pool.Exec(ctx, "SELECT count(*) FROM pg_stat_statements"); err != nil {
		return &Unsupported{Reason: "pg_stat_statements not loaded: " + err.Error()}
	}
	return nil
}

// tempFiles is this database's cumulative temp file count.
func tempFiles(ctx context.Context, e *Env) (int, error) {
	return e.count(ctx, `SELECT temp_files::int FROM pg_catalog.pg_stat_database
		WHERE datname = current_database()`)
}

// spillLoops starts one looping session per statement and waits until
// they have written temp files.
func spillLoops(app string, pace time.Duration, sqls ...string) program {
	var before int
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := pgss(ctx, e); err != nil {
				return err
			}
			var err error
			if before, err = tempFiles(ctx, e); err != nil {
				return err
			}
			for _, sql := range sqls {
				if _, err := e.loop(ctx, app, spillSetup, sql, pace); err != nil {
					return err
				}
			}
			return nil
		},
		manifest: func(ctx context.Context, e *Env) error {
			return waitLong(ctx, "repeated spills", func() (bool, error) {
				n, err := tempFiles(ctx, e)
				return n >= before+2*len(sqls), err
			})
		},
		valid: func(ctx context.Context, e *Env) error {
			return expectActive(ctx, e, app, len(sqls))
		},
		recover: func(ctx context.Context, e *Env) error {
			if err := gone(app)(ctx, e); err != nil {
				return err
			}
			return noLiveTemp(ctx, e)
		},
	}
}

// repeatedSpill: one statement spilling about 25 MB on every call.
func repeatedSpill() program {
	return spillLoops("bench_batch", 0, sortSQL(300000, "md5(g::text)"))
}

// workloadSpills: four different statements each spilling a few MB,
// paced alike so none dominates.
func workloadSpills() program {
	const rows = 60000
	return spillLoops("bench_app", 400*time.Millisecond, sortSQL(rows, "md5(g::text)"),
		sortSQL(rows, "md5(g::varchar)"), sortSQL(rows, "md5(g::text) DESC"),
		sortSQL(rows, "md5((g + 1)::text)"))
}

// finishedSpill (decoy of a runaway query): a large spill that finished
// before the investigation; only the cumulative counters remember it.
func finishedSpill() program {
	var before int
	return program{
		inject: func(ctx context.Context, e *Env) error {
			var err error
			if before, err = e.count(ctx, `SELECT (temp_bytes >> 20)::int
				FROM pg_catalog.pg_stat_database WHERE datname = current_database()`); err != nil {
				return err
			}
			return e.simple(ctx, "BEGIN; SET LOCAL work_mem = '1MB'; "+
				sortSQL(1500000, "md5(g::text)")+"; COMMIT")
		},
		manifest: func(ctx context.Context, e *Env) error {
			if err := waitLong(ctx, "the finished spill's statistics", func() (bool, error) {
				n, err := e.count(ctx, `SELECT (temp_bytes >> 20)::int
					FROM pg_catalog.pg_stat_database WHERE datname = current_database()`)
				return n >= before+64, err
			}); err != nil {
				return err
			}
			return noLiveTemp(ctx, e)
		},
		recover: noLiveTemp,
	}
}

// noLiveTemp checks that no backend of this database holds temp files.
func noLiveTemp(ctx context.Context, e *Env) error {
	return waitFor(ctx, "no live temp files", func() (bool, error) {
		b, err := liveTemp(ctx, e)
		return b == 0, err
	})
}
