package srebench

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Lock blocking fault programs (the M1 fixtures, run through the loop).

// Scenarios is the seed set, in run order.
func Scenarios() []Scenario {
	var out []Scenario
	for _, group := range [][]Scenario{lockScenarios(), connectionScenarios(),
		walScenarios(), planScenarios(), checkpointScenarios(), tempScenarios(),
		replicationScenarios(), lwlockScenarios(), wrapScenarios(), diskScenarios(),
		sequenceScenarios()} {
		out = append(out, group...)
	}
	return out
}

func lockScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerLock, Class: class, Gold: gold, Program: p}
}

func lockScenarios() []Scenario {
	idle, hot := Gold{Root: "idle_in_tx_holder"}, Gold{Root: "hot_row_contention"}
	return []Scenario{
		lockScenario("lock-idle-row-holder", ClassPositive, idle, idleRowHolder()),
		lockScenario("lock-idle-holder-ddl-queue", ClassPositive,
			Gold{Root: "idle_in_tx_holder", Contributing: []string{"ddl_lock_queue"}},
			queuedDDL("BEGIN; SELECT count(*) FROM %s;", "idle in transaction")),
		lockScenario("lock-ddl-behind-active", ClassPositive, Gold{Root: "ddl_lock_queue"},
			queuedDDL("SELECT count(*) FROM %s, pg_sleep(60)", "active")),
		lockScenario("lock-hot-row", ClassPositive, hot, hotRow()),
		lockScenario("lock-prepared-holder", ClassPositive,
			Gold{Root: "prepared_xact_holder"}, preparedHolder()),
		lockScenario("lock-idle-row-holder-under-load", ClassNoise, idle,
			withNoise(idleRowHolder())),
		lockScenario("lock-hot-row-under-load", ClassNoise, hot, withNoise(hotRow())),
		lockScenario("lock-transient-wait", ClassDecoy,
			Gold{Lookalike: "hot_row_contention"}, transientWait()),
		lockScenario("lock-idle-tx-blocking-nobody", ClassDecoy,
			Gold{Lookalike: "idle_in_tx_holder"}, idleBlockingNobody()),
		lockScenario("lock-no-waits", ClassBenign, Gold{}, noLockWaits()),
	}
}

// idleRowHolder: a session updates a row and stays idle in its
// transaction; two sessions wait for the row.
func idleRowHolder() program {
	tb := newTable("idle")
	update := "UPDATE %s SET v = v + 1 WHERE id = 1"
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			pid, err := e.background(ctx, "bench_holder",
				"BEGIN; "+fmt.Sprintf(update, tb.name)+";")
			if err != nil {
				return err
			}
			if err := e.sessionState(ctx, pid, "idle in transaction"); err != nil {
				return err
			}
			return waiters(ctx, e, fmt.Sprintf(update, tb.name), 2)
		},
		manifest: func(ctx context.Context, e *Env) error { return e.atLeastWaiters(ctx, 2) },
		recover:  tb.drop,
	}
}

func waiters(ctx context.Context, e *Env, sql string, n int) error {
	for i := 0; i < n; i++ {
		if _, err := e.background(ctx, "bench_waiter", sql); err != nil {
			return err
		}
		if err := e.atLeastWaiters(ctx, i+1); err != nil {
			return err
		}
	}
	return nil
}

// queuedDDL: holderSQL keeps a relation lock (idle or active), an ALTER
// TABLE queues behind it and a reader queues behind the ALTER.
func queuedDDL(holderSQL, holderState string) program {
	tb := newTable("ddl")
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			pid, err := e.background(ctx, "bench_holder", fmt.Sprintf(holderSQL, tb.name))
			if err != nil {
				return err
			}
			return e.sessionState(ctx, pid, holderState)
		},
		manifest: func(ctx context.Context, e *Env) error {
			if _, err := e.background(ctx, "bench_migration",
				"ALTER TABLE "+tb.name+" ADD COLUMN w int"); err != nil {
				return err
			}
			if err := e.atLeastWaiters(ctx, 1); err != nil {
				return err
			}
			if _, err := e.background(ctx, "bench_reader",
				"SELECT count(*) FROM "+tb.name); err != nil {
				return err
			}
			return e.atLeastWaiters(ctx, 2)
		},
		recover: tb.drop,
	}
}

// hotRow: an active transaction holds a row others update.
func hotRow() program {
	tb := newTable("hot")
	update := "UPDATE %s SET v = v + 1 WHERE id = 1"
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			holder := "BEGIN; " + fmt.Sprintf(update, tb.name) + "; SELECT pg_sleep(60); COMMIT;"
			if _, err := e.background(ctx, "bench_holder", holder); err != nil {
				return err
			}
			time.Sleep(200 * time.Millisecond)
			return waiters(ctx, e, fmt.Sprintf(update, tb.name), 3)
		},
		manifest: func(ctx context.Context, e *Env) error { return e.atLeastWaiters(ctx, 3) },
		recover:  tb.drop,
	}
}

// preparedHolder: a prepared (two-phase) transaction holds a row lock.
func preparedHolder() program {
	tb := newTable("prep")
	gid := fmt.Sprintf("bench_prepared_%d", time.Now().UnixNano())
	update := "UPDATE %s SET v = v + 1 WHERE id = 1"
	return program{
		inject: func(ctx context.Context, e *Env) error {
			var max int
			if err := e.Pool.QueryRow(ctx,
				"SELECT current_setting('max_prepared_transactions')::int").Scan(&max); err != nil {
				return err
			}
			if max == 0 {
				return &Unsupported{Reason: "max_prepared_transactions is 0"}
			}
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			if err := e.simple(ctx, "BEGIN; "+fmt.Sprintf(update, tb.name)+
				"; PREPARE TRANSACTION '"+gid+"'"); err != nil {
				return err
			}
			return waiters(ctx, e, fmt.Sprintf(update, tb.name), 2)
		},
		manifest: func(ctx context.Context, e *Env) error { return e.atLeastWaiters(ctx, 2) },
		recover: func(ctx context.Context, e *Env) error {
			_ = e.simple(ctx, "ROLLBACK PREPARED '"+gid+"'")
			n, err := e.count(ctx, "SELECT count(*) FROM pg_prepared_xacts WHERE gid = $1", gid)
			if err == nil && n != 0 {
				return fmt.Errorf("prepared transaction %s still exists", gid)
			}
			if err != nil {
				return err
			}
			return tb.drop(ctx, e)
		},
	}
}

// noLockWaits: a table and an idle session, nothing blocked.
func noLockWaits() program {
	tb := newTable("calm")
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			return e.idle(ctx, "bench_calm", 2)
		},
		manifest: func(ctx context.Context, e *Env) error {
			n, err := e.lockWaiters(ctx)
			if err == nil && n != 0 {
				err = fmt.Errorf("%d lock waiters before a calm run", n)
			}
			return err
		},
		recover: tb.drop,
	}
}

// noWaiters waits until no session of this database waits on a lock.
func noWaiters(ctx context.Context, e *Env) error {
	return waitFor(ctx, "lock waits to resolve", func() (bool, error) {
		n, err := e.lockWaiters(ctx)
		return n == 0, err
	})
}

// transientWait (decoy of hot-row contention): a row lock wait that
// resolves after two seconds, before the investigation starts. The wait
// is observed during injection; the manifestation predicate is that it
// resolved.
func transientWait() program {
	tb := newTable("transient")
	update := "UPDATE %s SET v = v + 1 WHERE id = 1"
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			pid, err := e.background(ctx, "bench_holder", "BEGIN; "+
				fmt.Sprintf(update, tb.name)+"; SELECT pg_sleep(2); COMMIT;")
			if err != nil {
				return err
			}
			if err := e.sleeping(ctx, pid); err != nil {
				return err
			}
			return waiters(ctx, e, fmt.Sprintf(update, tb.name), 1)
		},
		manifest: noWaiters,
		recover:  tb.drop,
	}
}

// idleBlockingNobody (decoy of an idle-in-transaction holder): a session
// holds a row lock idle in its transaction, and nobody waits for it.
func idleBlockingNobody() program {
	tb := newTable("idlenobody")
	var pid int
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := tb.create(ctx, e); err != nil {
				return err
			}
			var err error
			pid, err = e.background(ctx, "bench_holder",
				"BEGIN; UPDATE "+tb.name+" SET v = v + 1 WHERE id = 1;")
			if err != nil {
				return err
			}
			return e.sessionState(ctx, pid, "idle in transaction")
		},
		manifest: func(ctx context.Context, e *Env) error {
			if err := e.sessionState(ctx, pid, "idle in transaction"); err != nil {
				return err
			}
			return noWaiters(ctx, e)
		},
		recover: tb.drop,
	}
}
