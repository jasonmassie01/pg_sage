package srebench

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Connection pressure fault programs: a stable idle pool per application
// (fan-out), a pool growing between the samples (leak) and sessions
// piling up behind a lock (backlog); their noise variants; and decoys:
// several small pools that add up, and a pool warming up by less than a
// leak's growth.

func connScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerConnections, Class: class, Gold: gold,
		Program: p}
}

func connectionScenarios() []Scenario {
	fanOut, leaking := Gold{Root: "pool_fan_out"}, Gold{Root: "connection_leak"}
	return []Scenario{
		connScenario("conn-pool-fan-out", ClassPositive, fanOut, idlePool(15, false)),
		connScenario("conn-leak", ClassPositive, leaking, leak()),
		connScenario("conn-blocked-backlog", ClassPositive,
			Gold{Root: "blocked_backlog"}, backlog()),
		connScenario("conn-fan-out-after-own-action", ClassNoise, fanOut,
			idlePool(15, true)),
		connScenario("conn-leak-under-load", ClassNoise, leaking, withNoise(leak())),
		connScenario("conn-bounded-pools", ClassDecoy, Gold{Lookalike: "pool_fan_out"},
			smallPools()),
		connScenario("conn-pool-warm-up", ClassDecoy, Gold{Lookalike: "connection_leak"},
			warmUp()),
		connScenario("conn-quiet", ClassBenign, Gold{}, program{manifest: calm,
			recover: calm}),
	}
}

// calm checks that no session of this database waits on a lock.
func calm(ctx context.Context, e *Env) error {
	n, err := e.lockWaiters(ctx)
	if err == nil && n != 0 {
		err = fmt.Errorf("%d lock waiters on a calm database", n)
	}
	return err
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

// boundedPoolApps each keep a pool under the fan-out threshold; together
// they hold more idle backends than the fan-out scenario's one pool.
var boundedPoolApps = []string{"bench_api_a", "bench_api_b", "bench_api_c"}

// smallPools (decoy of pool fan-out): three applications with six idle
// backends each, stable.
func smallPools() program {
	each := func(f func(string) func(context.Context, *Env) error) func(context.Context,
		*Env) error {
		return func(ctx context.Context, e *Env) error {
			for _, app := range boundedPoolApps {
				if err := f(app)(ctx, e); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return program{
		inject: each(func(app string) func(context.Context, *Env) error {
			return func(ctx context.Context, e *Env) error { return e.idle(ctx, app, 6) }
		}),
		manifest: each(func(app string) func(context.Context, *Env) error {
			return expectIdle(app, 6)
		}),
		recover: each(func(app string) func(context.Context, *Env) error {
			return expectIdle(app, 0)
		}),
	}
}

// warmUp (decoy of a connection leak): a pool of 5 opens 2 more between
// the samples, under the 3-backend growth a leak needs.
func warmUp() program {
	const app = "bench_pool"
	return program{
		inject:   func(ctx context.Context, e *Env) error { return e.idle(ctx, app, 5) },
		manifest: expectIdle(app, 5),
		between: func(ctx context.Context, e *Env) error {
			if err := e.idle(ctx, app, 2); err != nil {
				return err
			}
			return expectIdle(app, 7)(ctx, e)
		},
		recover: expectIdle(app, 0),
	}
}
