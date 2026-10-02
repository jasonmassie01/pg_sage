package srebench

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Sequence exhaustion fault programs: a sequence owned by a table's id
// column, set near (or far from) its limit, consuming values while the
// runway is sampled and between the investigation's samples. Sequences
// are per database, so these take no cluster lock.

const int4Max = 2147483647

// seqFixture describes one scenario's sequence.
type seqFixture struct {
	name     string // the table; its sequence is name_seq
	seqType  string // the sequence's type
	colType  string // the owning column's type
	maxValue int64  // an explicit MAXVALUE (0: the type's)
	start    int64  // setval before sampling
	cycle    bool
	consume  int // values consumed per step (0: dormant)
}

func (f seqFixture) seq() string     { return f.name + "_seq" }
func (f seqFixture) subject() string { return "sequence public." + f.seq() }

func (f seqFixture) ddl() string {
	opts := "AS " + f.seqType
	if f.maxValue > 0 {
		opts += fmt.Sprintf(" MAXVALUE %d", f.maxValue)
	}
	if f.cycle {
		opts += " CYCLE"
	}
	return fmt.Sprintf(`CREATE SEQUENCE %[1]s %[2]s;
		CREATE TABLE %[3]s (id %[4]s DEFAULT nextval('%[1]s'), pad int);
		ALTER SEQUENCE %[1]s OWNED BY %[3]s.id;
		SELECT setval('%[1]s', %[5]d)`, f.seq(), opts, f.name, f.colType, f.start)
}

func (f seqFixture) consumeSQL() string {
	return fmt.Sprintf("INSERT INTO %s (pad) SELECT 1 FROM generate_series(1, %d)", f.name,
		f.consume)
}

func seqScenario(id, class string, gold Gold, f seqFixture, wrap func(program) program) Scenario {
	p := seqProgram(f)
	if wrap != nil {
		p = wrap(p)
	}
	return Scenario{ID: id, Family: sre.TriggerSequence, Class: class, Gold: gold,
		Subject: f.subject(), Program: p}
}

func sequenceScenarios() []Scenario {
	near := int64(int4Max - 200000)
	typeLimit := seqFixture{name: uniqueName("bench_seq_"), seqType: "integer",
		colType: "int", start: near, consume: 1000}
	narrow := seqFixture{name: uniqueName("bench_seq_"), seqType: "bigint", colType: "int",
		start: near, consume: 1000}
	capped := seqFixture{name: uniqueName("bench_seq_"), seqType: "bigint",
		colType: "bigint", maxValue: 1000000, start: 900000, consume: 500}
	loadedType, loadedNarrow := typeLimit, narrow
	loadedType.name, loadedNarrow.name = uniqueName("bench_seq_"), uniqueName("bench_seq_")
	dormant := seqFixture{name: uniqueName("bench_seq_"), seqType: "integer",
		colType: "int", start: near}
	cycling := seqFixture{name: uniqueName("bench_seq_"), seqType: "integer",
		colType: "int", start: near, cycle: true, consume: 1000}
	far := seqFixture{name: uniqueName("bench_seq_"), seqType: "integer", colType: "int",
		start: 1000, consume: 100}
	lookalike := Gold{Lookalike: "sequence_type_limit"}
	return []Scenario{
		seqScenario("seq-type-limit", ClassPositive, Gold{Root: "sequence_type_limit"},
			typeLimit, nil),
		seqScenario("seq-narrow-column", ClassPositive,
			Gold{Root: "column_narrower_than_sequence"}, narrow, nil),
		seqScenario("seq-explicit-maxvalue", ClassPositive,
			Gold{Root: "explicit_maxvalue_limit"}, capped, nil),
		seqScenario("seq-type-limit-under-load", ClassNoise,
			Gold{Root: "sequence_type_limit"}, loadedType, withNoise),
		seqScenario("seq-narrow-column-under-load", ClassNoise,
			Gold{Root: "column_narrower_than_sequence"}, loadedNarrow, withNoise),
		seqScenario("seq-dormant-near-limit", ClassDecoy, lookalike, dormant, nil),
		seqScenario("seq-cycling-near-limit", ClassDecoy, lookalike, cycling, nil),
		seqScenario("seq-far-from-limit", ClassBenign, Gold{}, far, nil),
	}
}

// seqProgram creates the sequence, samples its runway while it consumes,
// consumes again between the investigation's samples, and drops it.
func seqProgram(f seqFixture) program {
	consume := func(ctx context.Context, e *Env) error {
		if f.consume == 0 {
			return nil
		}
		_, err := e.Pool.Exec(ctx, f.consumeSQL())
		return err
	}
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := e.simple(ctx, f.ddl()); err != nil {
				return err
			}
			return e.sampleRunway(ctx, func(ctx context.Context, _ int) error {
				return consume(ctx, e)
			})
		},
		manifest: func(ctx context.Context, e *Env) error { return seqManifest(ctx, e, f) },
		between:  consume,
		recover: func(ctx context.Context, e *Env) error {
			if _, err := e.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+f.name); err != nil {
				return err
			}
			n, err := e.count(ctx, "SELECT count(*) FROM pg_class WHERE relname = $1",
				f.seq())
			if err == nil && n != 0 {
				err = fmt.Errorf("sequence %s survived its table", f.seq())
			}
			if err != nil {
				return err
			}
			return e.clearRunway(ctx)
		},
	}
}

// seqManifest: the sequence advanced exactly when it consumes, and sits
// near its limit unless the scenario starts far from it.
func seqManifest(ctx context.Context, e *Env, f seqFixture) error {
	var last int64
	var fraction float64
	if err := e.Pool.QueryRow(ctx, `SELECT last_value,
		(last_value - min_value)::float8 / NULLIF(LEAST(max_value, $2) - min_value, 0)
		FROM pg_sequences WHERE schemaname = 'public' AND sequencename = $1`, f.seq(),
		int4Max).Scan(&last, &fraction); err != nil {
		return err
	}
	if f.maxValue > 0 {
		fraction = float64(last-1) / float64(f.maxValue-1)
	}
	advanced := last > f.start
	switch {
	case advanced != (f.consume > 0):
		return fmt.Errorf("sequence %s at %d: advanced=%v, consumes %d", f.seq(), last,
			advanced, f.consume)
	case f.start < int4Max/2 && f.maxValue == 0 && fraction >= 0.05:
		return fmt.Errorf("sequence %s is not far from its limit (%v)", f.seq(), fraction)
	case (f.start >= int4Max/2 || f.maxValue > 0) && fraction < 0.9:
		return fmt.Errorf("sequence %s is not near its limit (%v)", f.seq(), fraction)
	}
	return nil
}
