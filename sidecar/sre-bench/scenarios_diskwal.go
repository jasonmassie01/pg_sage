package srebench

import (
	"context"
	"fmt"
	"math"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Disk / WAL runway fault programs. Slots and WAL are cluster-wide, so
// each takes the cluster fixture lock on a clean cluster (walProgram).
// The runway is sampled while the fault fills: WAL retained by a slot
// (logical messages, which grow no table) or a table growing. Between the
// investigation's samples nothing is written: a slow fill is invisible in
// a few seconds, so the trend must carry it. Other test packages creating
// or dropping fixture databases change the cluster's database size; such
// a run is contaminated and repeated.

const (
	fillMiB       = 6  // WAL (MiB) a slot retains more per runway step
	growMiB       = 10 // MiB a table grows per runway step
	otherDBsDrift = 4 << 20
)

func diskScenario(id, class string, gold Gold, p program) Scenario {
	return Scenario{ID: id, Family: sre.TriggerDiskWAL, Class: class, Gold: gold,
		Subject: "disk", Program: p}
}

func diskScenarios() []Scenario {
	growth := Gold{Root: "database_growth"}
	return []Scenario{
		diskScenario("disk-inactive-slot-fill", ClassPositive, Gold{Root: "inactive_slot"},
			slotFill(false)),
		diskScenario("disk-slow-consumer-fill", ClassPositive, Gold{Root: "slow_consumer"},
			slotFill(true)),
		diskScenario("disk-database-growth", ClassPositive, growth, tableGrowth(false)),
		diskScenario("disk-inactive-slot-fill-under-load", ClassNoise,
			Gold{Root: "inactive_slot"}, withNoise(slotFill(false))),
		diskScenario("disk-database-growth-under-load", ClassNoise, growth,
			withNoise(tableGrowth(false))),
		diskScenario("disk-churn", ClassDecoy, Gold{Lookalike: "database_growth"},
			tableGrowth(true)),
		diskScenario("disk-slot-keeping-up", ClassDecoy, Gold{Lookalike: "slow_consumer"},
			keepingUpFill()),
		diskScenario("disk-steady", ClassBenign, Gold{}, steadyDisk()),
	}
}

// stableDatabases wraps a program so a run in which other databases
// changed size is contaminated.
func stableDatabases(p program) program {
	var before float64
	inject := p.inject
	p.inject = func(ctx context.Context, e *Env) error {
		var err error
		if before, err = e.otherDatabasesBytes(ctx); err != nil {
			return err
		}
		return step(inject, ctx, e)
	}
	valid := p.valid
	p.valid = func(ctx context.Context, e *Env) error {
		after, err := e.otherDatabasesBytes(ctx)
		if err != nil {
			return err
		}
		if drift := math.Abs(after - before); drift > otherDBsDrift {
			return &Contaminated{Reason: fmt.Sprintf("other databases changed by %.0f "+
				"bytes during the run", drift)}
		}
		return step(valid, ctx, e)
	}
	return p
}

// slotFill: a slot (inactive physical, or logical with a consumer that
// never confirms) retains the WAL written while the runway is sampled.
func slotFill(active bool) program {
	slot := uniqueName(slotPrefix)
	var c *consumer
	p := walProgram(func(ctx context.Context, e *Env) error {
		if err := createFillSlot(ctx, e, slot, active, &c); err != nil {
			return err
		}
		return e.sampleRunway(ctx, func(ctx context.Context, _ int) error {
			return e.emitWAL(ctx, fillMiB)
		})
	}, nil)
	p.manifest = func(ctx context.Context, e *Env) error {
		if err := slotActive(slot, active)(ctx, e); err != nil {
			return err
		}
		return slotRetains(ctx, e, slot, 3*fillMiB<<20)
	}
	p.valid = func(context.Context, *Env) error { return consumerAlive(c) }
	p.recover = recoverSlot(&c, slot, p.recover)
	return stableDatabases(p)
}

// slotRetains checks that slot retains at least want bytes of written
// WAL (pg_current_wal_lsn, the write position); a shortfall says how much
// it retains.
func slotRetains(ctx context.Context, e *Env, slot string, want int64) error {
	var retained *int64
	err := e.Pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(),
		restart_lsn)::int8 FROM pg_replication_slots WHERE slot_name = $1`, slot).
		Scan(&retained)
	switch {
	case err != nil:
		return fmt.Errorf("slot %s: %w", slot, err)
	case retained == nil:
		return fmt.Errorf("slot %s reserves no WAL", slot)
	case *retained < want:
		return fmt.Errorf("slot %s does not retain the written WAL: %d bytes, want %d",
			slot, *retained, want)
	}
	return nil
}

func createFillSlot(ctx context.Context, e *Env, slot string, active bool,
	c **consumer) error {
	if !active {
		_, err := e.Pool.Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true)",
			slot)
		return err
	}
	if err := createLogicalSlot(ctx, e, slot); err != nil {
		return err
	}
	var err error
	*c, err = startLogicalConsumer(ctx, e, slot, false)
	return err
}

// tableGrowth grows a table while the runway is sampled; with churn it
// alternately loads and truncates it, so its size goes up and down.
func tableGrowth(churn bool) program {
	tb := newTable("grow")
	p := walProgram(func(ctx context.Context, e *Env) error {
		if _, err := e.Pool.Exec(ctx, "CREATE TABLE "+tb.name+" (pad text)"); err != nil {
			return err
		}
		return e.sampleRunway(ctx, func(ctx context.Context, i int) error {
			if churn && i%2 == 0 {
				_, err := e.Pool.Exec(ctx, "TRUNCATE "+tb.name)
				return err
			}
			_, err := e.Pool.Exec(ctx, "INSERT INTO "+tb.name+
				" SELECT repeat(md5(g::text), 32) FROM generate_series(1, $1) g",
				growMiB*1024)
			return err
		})
	}, nil)
	p.manifest = func(ctx context.Context, e *Env) error {
		var size int64
		if err := e.Pool.QueryRow(ctx, "SELECT pg_total_relation_size($1::regclass)",
			tb.name).Scan(&size); err != nil {
			return err
		}
		if want := int64(growMiB << 20); churn != (size < 2*want) {
			return fmt.Errorf("table %s is %d bytes (churn %v)", tb.name, size, churn)
		}
		return cleanCluster(ctx, e)
	}
	walRecover := p.recover
	p.recover = func(ctx context.Context, e *Env) error {
		if _, err := e.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+tb.name); err != nil {
			return err
		}
		return step(walRecover, ctx, e)
	}
	return stableDatabases(p)
}

// keepingUpFill (decoy of a slow consumer): a logical slot whose consumer
// confirms what it receives while WAL is written.
func keepingUpFill() program {
	slot := uniqueName(slotPrefix)
	var c *consumer
	p := walProgram(func(ctx context.Context, e *Env) error {
		if err := createLogicalSlot(ctx, e, slot); err != nil {
			return err
		}
		var err error
		if c, err = startLogicalConsumer(ctx, e, slot, true); err != nil {
			return err
		}
		return e.sampleRunway(ctx, func(ctx context.Context, _ int) error {
			return keepsUp(ctx, e, slot)
		})
	}, nil)
	p.manifest = func(ctx context.Context, e *Env) error {
		if err := slotActive(slot, true)(ctx, e); err != nil {
			return err
		}
		return keepsUp(ctx, e, slot)
	}
	p.valid = func(context.Context, *Env) error { return consumerAlive(c) }
	p.recover = recoverSlot(&c, slot, p.recover)
	return stableDatabases(p)
}

// steadyDisk: nothing fills on a clean cluster.
func steadyDisk() program {
	p := walProgram(func(ctx context.Context, e *Env) error {
		return e.sampleRunway(ctx, nil)
	}, nil)
	p.manifest = cleanCluster
	return stableDatabases(p)
}
