package srebench

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Connection pressure fault programs: a stable idle pool per application
// (fan-out), a pool growing between the samples (leak) and sessions
// piling up behind a lock (backlog).

func connScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerConnections, Class: class, Gold: gold,
		Program: p}
}

func connectionScenarios() []Scenario {
	fanOut := Gold{Root: "pool_fan_out"}
	return []Scenario{
		connScenario("conn-pool-fan-out", ClassPositive, fanOut, idlePool(15, false)),
		connScenario("conn-leak", ClassPositive, Gold{Root: "connection_leak"}, leak()),
		connScenario("conn-blocked-backlog", ClassPositive,
			Gold{Root: "blocked_backlog"}, backlog()),
		connScenario("conn-quiet", ClassBenign, Gold{}, program{}),
		connScenario("conn-fan-out-after-own-action", ClassDecoy, fanOut,
			idlePool(15, true)),
	}
}

func idleCount(ctx context.Context, e *Env, app string) (int, error) {
	return e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND application_name = $1
		  AND state = 'idle'`, app)
}

func expectIdle(app string, want int) func(context.Context, *Env) error {
	return func(ctx context.Context, e *Env) error {
		return waitFor(ctx, fmt.Sprintf("%d idle %s backends", want, app),
			func() (bool, error) {
				n, err := idleCount(ctx, e, app)
				return n == want, err
			})
	}
}

// idlePool opens n idle connections of one application and keeps them.
// withAction also records a recent pg_sage action (a decoy "change").
func idlePool(n int, withAction bool) program {
	const app = "bench_api"
	var action int64
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if withAction {
				if err := e.Pool.QueryRow(ctx, `INSERT INTO sage.action_log
					(action_type, sql_executed, outcome)
					VALUES ('analyze', 'ANALYZE bench', 'success') RETURNING id`).
					Scan(&action); err != nil {
					return err
				}
			}
			return e.idle(ctx, app, n)
		},
		manifest: expectIdle(app, n),
		recover: func(ctx context.Context, e *Env) error {
			if action != 0 {
				if _, err := e.Pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id = $1",
					action); err != nil {
					return err
				}
			}
			return expectIdle(app, 0)(ctx, e)
		},
	}
}

// leak: a pool of 4 grows by 9 between the samples.
func leak() program {
	const app = "bench_worker"
	return program{
		inject:   func(ctx context.Context, e *Env) error { return e.idle(ctx, app, 4) },
		manifest: expectIdle(app, 4),
		between: func(ctx context.Context, e *Env) error {
			if err := e.idle(ctx, app, 9); err != nil {
				return err
			}
			return expectIdle(app, 13)(ctx, e)
		},
		recover: expectIdle(app, 0),
	}
}

// backlog: an idle transaction holds ACCESS EXCLUSIVE; six readers pile
// up behind it with their connections.
func backlog() program {
	tb := newTable("backlog")
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			pid, err := e.background(ctx, "bench_holder",
				"BEGIN; LOCK TABLE "+tb.name+" IN ACCESS EXCLUSIVE MODE;")
			if err != nil {
				return err
			}
			if err := e.sessionState(ctx, pid, "idle in transaction"); err != nil {
				return err
			}
			return waiters(ctx, e, "SELECT count(*) FROM "+tb.name, 6)
		},
		manifest: func(ctx context.Context, e *Env) error { return e.atLeastWaiters(ctx, 6) },
		recover:  tb.drop,
	}
}
