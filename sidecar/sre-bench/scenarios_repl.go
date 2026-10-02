package srebench

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Replication lag fault programs. The replica is a logical replication
// consumer on a test_decoding slot: pg_stat_replication shows its
// positions as it confirms them, so a consumer that confirms writes but
// not replay, or writes but not flushes, puts the lag at one stage. WAL
// and walsenders are cluster-wide: each program takes the cluster lock
// and needs a cluster without slots or replicas.
//
// WAL not leaving the primary (wal_send_backlog) has no fault program:
// it needs network backpressure on the walsender, and a consumer that
// stops reading behind Docker's port proxy does not create it (the proxy
// buffers what the walsender sends). The matcher covers it in unit tests.

func replScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerReplicationLag, Class: class, Gold: gold,
		Program: p}
}

func replicationScenarios() []Scenario {
	surge := []string{"replication_write_surge"}
	replay := Gold{Root: "standby_replay_backlog", Contributing: surge}
	return []Scenario{
		replScenario("repl-replay-backlog", ClassPositive, replay,
			laggingReplica(ackWriteFlush, 48, "flush_lsn")),
		replScenario("repl-flush-backlog", ClassPositive,
			Gold{Root: "standby_flush_backlog", Contributing: surge},
			laggingReplica(ackWrite, 48, "write_lsn")),
		replScenario("repl-replay-backlog-under-load", ClassNoise, replay,
			withNoise(laggingReplica(ackWriteFlush, 48, "flush_lsn"))),
		replScenario("repl-keeping-up", ClassDecoy,
			Gold{Lookalike: "standby_replay_backlog"},
			laggingReplica(ackAll, 48, "replay_lsn")),
		replScenario("repl-no-replicas", ClassBenign, Gold{}, noReplicas()),
	}
}

// replicas counts the cluster's replication connections.
func replicas(ctx context.Context, e *Env) (int, error) {
	return e.count(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_replication")
}

// replicaProgram creates a slot and a consumer in mode on a cluster
// without slots or replicas, and checks it confirmed its start.
func replicaProgram(mode ackMode) (program, *string, **consumer) {
	slot := uniqueName(slotPrefix)
	var c *consumer
	p := walProgram(func(ctx context.Context, e *Env) error {
		if n, err := replicas(ctx, e); err != nil || n > 0 {
			return fmt.Errorf("cluster not clean: %d replication connections (%v)", n, err)
		}
		if err := createLogicalSlot(ctx, e, slot); err != nil {
			return err
		}
		var err error
		c, err = startConsumer(ctx, e, slot, mode)
		return err
	}, nil)
	p.manifest = func(ctx context.Context, e *Env) error {
		if err := slotActive(slot, true)(ctx, e); err != nil {
			return err
		}
		return confirmedStart(ctx, e, slot)
	}
	p.valid = func(context.Context, *Env) error { return consumerAlive(c) }
	p.recover = recoverSlot(&c, slot, p.recover)
	return p, &slot, &c
}

// confirmedStart writes a little WAL in this database (the consumer's
// first message) and waits until the replica reports a write position.
func confirmedStart(ctx context.Context, e *Env, slot string) error {
	if _, err := e.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS bench_wal (id int, pad text);
		INSERT INTO bench_wal SELECT g, 'x' FROM generate_series(1, 10) g`); err != nil {
		return err
	}
	return waitFor(ctx, "the replica's first status", func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_replication r
			JOIN pg_catalog.pg_replication_slots s ON s.active_pid = r.pid
			WHERE s.slot_name = $1 AND r.write_lsn IS NOT NULL`, slot)
		return n == 1, err
	})
}

// laggingReplica writes mb MiB of WAL at each sample interval and waits
// until the replica's position column has reached it; the positions the
// mode does not confirm fall behind.
func laggingReplica(mode ackMode, mb int, reached string) program {
	p, slot, _ := replicaProgram(mode)
	p.between = func(ctx context.Context, e *Env) error {
		if err := writeWAL(mb)(ctx, e); err != nil {
			return err
		}
		return replicaReached(ctx, e, *slot, reached)
	}
	return p
}

// positionColumns are the pg_stat_replication positions a program waits on.
var positionColumns = map[string]bool{"write_lsn": true, "flush_lsn": true,
	"replay_lsn": true}

// replicaReached waits until the slot's replica reports column (a fixed
// pg_stat_replication position name) at the current WAL position.
func replicaReached(ctx context.Context, e *Env, slot, column string) error {
	if !positionColumns[column] {
		return fmt.Errorf("%q is not a replica position column", column)
	}
	var target string
	if err := e.Pool.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").
		Scan(&target); err != nil {
		return err
	}
	return waitLong(ctx, "replica "+column, func() (bool, error) {
		n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_replication r
			JOIN pg_catalog.pg_replication_slots s ON s.active_pid = r.pid
			WHERE s.slot_name = $1 AND r.`+column+` >= $2::pg_lsn`, slot, target)
		return n == 1, err
	})
}

// noReplicas: a cluster without replicas or slots.
func noReplicas() program {
	none := func(ctx context.Context, e *Env) error {
		n, err := replicas(ctx, e)
		if err == nil && n > 0 {
			err = &Contaminated{Reason: fmt.Sprintf("%d replication connections appeared", n)}
		}
		return err
	}
	p := walProgram(nil, nil)
	p.manifest = func(ctx context.Context, e *Env) error {
		if err := cleanCluster(ctx, e); err != nil {
			return err
		}
		return none(ctx, e)
	}
	p.valid = none
	return p
}
