package srebench

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// Wraparound runway fault programs. A table's reloption lowers its freeze
// maximum to 100,000 (the smallest PostgreSQL allows), so burning XIDs
// for a second brings it near or past its maximum; a holder of the xmin
// horizon, when there is one, starts before the burn and keeps the
// table's horizon from advancing.

const wrapFreezeMax = 100000

// wrapFixture describes one scenario.
type wrapFixture struct {
	table      string
	lowMax     bool   // reloption autovacuum_freeze_max_age = wrapFreezeMax
	holder     string // "session", "prepared", "idle" (no horizon) or ""
	preBurn    int    // XIDs burned before sampling
	sampleBurn int    // XIDs burned between runway samples
	surge      int    // XIDs burned between the investigation's samples
	gid        string
}

const (
	holderSession  = "session"
	holderPrepared = "prepared"
	holderIdle     = "idle"
	// quietXIDRate bounds other sessions' XID use during a run whose
	// premise is a quiet (or only self-made) XID rate.
	quietXIDRate = 300
)

func (f wrapFixture) subject() string {
	if f.lowMax {
		return "table public." + f.table
	}
	return "xid"
}

func wrapScenario(id, class string, gold Gold, f wrapFixture,
	wrap func(program) program) Scenario {
	p := wrapProgram(f)
	if wrap != nil {
		p = wrap(p)
	}
	return Scenario{ID: id, Family: sre.TriggerWraparound, Class: class, Gold: gold,
		Subject: f.subject(), Program: p}
}

func wrapScenarios() []Scenario {
	held := func(holder string) wrapFixture {
		return wrapFixture{table: uniqueName("bench_wrap_"), lowMax: true, holder: holder,
			sampleBurn: 45000, gid: uniqueName("bench_gid_")}
	}
	session := Gold{Root: "xmin_held_by_session"}
	return []Scenario{
		wrapScenario("wrap-session-holder", ClassPositive, session, held(holderSession), nil),
		wrapScenario("wrap-prepared-holder", ClassPositive,
			Gold{Root: "xmin_held_by_prepared_xact"}, held(holderPrepared), nil),
		wrapScenario("wrap-xid-surge", ClassPositive, Gold{Root: "xid_consumption_surge"},
			wrapFixture{table: uniqueName("bench_wrap_"), lowMax: true, preBurn: 90000,
				surge: 50000}, nil),
		wrapScenario("wrap-session-holder-under-load", ClassNoise, session,
			held(holderSession), withNoise),
		wrapScenario("wrap-idle-without-horizon", ClassDecoy,
			Gold{Lookalike: "xmin_held_by_session"},
			wrapFixture{table: uniqueName("bench_wrap_"), lowMax: true, holder: holderIdle,
				preBurn: 90000}, nil),
		wrapScenario("wrap-healthy", ClassBenign, Gold{},
			wrapFixture{table: uniqueName("bench_wrap_"), sampleBurn: 300}, nil),
	}
}

// wrapProgram creates the table and the holder, burns the XIDs and
// samples the runway, then burns the surge between the investigation's
// samples. A run in which other sessions used XIDs fast is contaminated.
func wrapProgram(f wrapFixture) program {
	var from int64
	var at time.Time
	return program{
		inject: func(ctx context.Context, e *Env) error {
			if err := wrapSetup(ctx, e, f); err != nil {
				return err
			}
			return e.sampleRunway(ctx, func(ctx context.Context, _ int) error {
				return e.burnXIDs(ctx, f.sampleBurn)
			})
		},
		manifest: func(ctx context.Context, e *Env) error { return wrapManifest(ctx, e, f) },
		between: func(ctx context.Context, e *Env) error {
			var err error
			if from, err = e.nextXID(ctx); err != nil {
				return err
			}
			at = time.Now()
			return e.burnXIDs(ctx, f.surge)
		},
		valid: func(ctx context.Context, e *Env) error {
			rate, err := e.xidRate(ctx, from+int64(f.surge), at)
			if err == nil && rate > quietXIDRate {
				return &Contaminated{Reason: fmt.Sprintf("other sessions used %.0f XIDs/s "+
					"during the investigation", rate)}
			}
			return err
		},
		recover: func(ctx context.Context, e *Env) error { return wrapRecover(ctx, e, f) },
	}
}

func wrapSetup(ctx context.Context, e *Env, f wrapFixture) error {
	ddl := "CREATE TABLE " + f.table + " (id int)"
	if f.lowMax {
		ddl += fmt.Sprintf(" WITH (autovacuum_freeze_max_age = %d)", wrapFreezeMax)
	}
	if _, err := e.Pool.Exec(ctx, ddl); err != nil {
		return err
	}
	if err := e.burnXIDs(ctx, f.preBurn); err != nil {
		return err
	}
	switch f.holder {
	case holderSession:
		return e.holdTransaction(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ")
	case holderIdle:
		return e.holdTransaction(ctx, "BEGIN")
	case holderPrepared:
		return e.prepareTransaction(ctx, f.gid)
	}
	return nil
}

// holdTransaction leaves a tracked session idle in a transaction opened
// with begin (REPEATABLE READ holds its snapshot; READ COMMITTED does not).
func (e *Env) holdTransaction(ctx context.Context, begin string) error {
	s, err := e.connect(ctx, "bench_wrap_holder")
	if err != nil {
		return err
	}
	defer close(s.done)
	if _, err := s.conn.Exec(ctx, begin); err != nil {
		return err
	}
	_, err = s.conn.Exec(ctx, "SELECT 1")
	return err
}

// prepareTransaction leaves a prepared transaction holding an XID.
func (e *Env) prepareTransaction(ctx context.Context, gid string) error {
	n, err := e.count(ctx, "SELECT current_setting('max_prepared_transactions')::int")
	if err != nil {
		return err
	}
	if n == 0 {
		return &Unsupported{Reason: "max_prepared_transactions = 0"}
	}
	s, err := e.connect(ctx, "bench_wrap_prepare")
	if err != nil {
		return err
	}
	defer close(s.done)
	for _, sql := range []string{"BEGIN", "SELECT pg_current_xact_id()",
		"PREPARE TRANSACTION '" + gid + "'"} {
		if _, err := s.conn.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return nil
}

// wrapManifest: a held table is past its maximum and its holder pins it;
// an unheld table sits between half its maximum and its maximum (other
// sessions' XIDs pushing it past are a contaminated run); the decoy's
// idle transaction holds no horizon.
func wrapManifest(ctx context.Context, e *Env, f wrapFixture) error {
	var age int64
	if err := e.Pool.QueryRow(ctx, `SELECT age(relfrozenxid)::int8 FROM pg_class
		WHERE oid = to_regclass($1)`, f.table).Scan(&age); err != nil {
		return err
	}
	held := f.holder == holderSession || f.holder == holderPrepared
	switch {
	case f.lowMax && held && age < wrapFreezeMax:
		return fmt.Errorf("held table %s is at age %d, under its maximum", f.table, age)
	case f.lowMax && !held && age >= wrapFreezeMax:
		return &Contaminated{Reason: fmt.Sprintf("table %s reached age %d before the "+
			"investigation", f.table, age)}
	case f.lowMax && !held && age < wrapFreezeMax/2:
		return fmt.Errorf("table %s is at age %d, not near its maximum", f.table, age)
	}
	return wrapHolderPresent(ctx, e, f, age)
}

func wrapHolderPresent(ctx context.Context, e *Env, f wrapFixture, age int64) error {
	var sql string
	var args []any
	switch f.holder {
	case holderSession:
		sql = `SELECT count(*) FROM pg_stat_activity WHERE application_name =
			'bench_wrap_holder' AND age(backend_xmin) >= $1`
		args = []any{age / 2}
	case holderIdle:
		sql = `SELECT count(*) FROM pg_stat_activity WHERE application_name =
			'bench_wrap_holder' AND state = 'idle in transaction'
			AND backend_xmin IS NULL AND backend_xid IS NULL`
	case holderPrepared:
		sql, args = "SELECT count(*) FROM pg_prepared_xacts WHERE gid = $1", []any{f.gid}
	default:
		sql = `SELECT 1 - count(*) FROM pg_prepared_xacts
			WHERE database = current_database()`
	}
	n, err := e.count(ctx, sql, args...)
	if err == nil && n != 1 {
		err = fmt.Errorf("holder %q of %s not as the scenario needs", f.holder, f.table)
	}
	return err
}

// wrapRecover resolves the prepared transaction, drops the table and the
// runway samples, and checks both are gone.
func wrapRecover(ctx context.Context, e *Env, f wrapFixture) error {
	if f.holder == holderPrepared {
		n, err := e.count(ctx, "SELECT count(*) FROM pg_prepared_xacts WHERE gid = $1", f.gid)
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err := e.Pool.Exec(ctx, "ROLLBACK PREPARED '"+f.gid+"'"); err != nil {
				return err
			}
		}
	}
	if _, err := e.Pool.Exec(ctx, "DROP TABLE IF EXISTS "+f.table); err != nil {
		return err
	}
	n, err := e.count(ctx, `SELECT (SELECT count(*) FROM pg_prepared_xacts WHERE gid = $1)
		+ (SELECT count(*) FROM pg_class WHERE oid = to_regclass($2))`, f.gid, f.table)
	if err == nil && n != 0 {
		err = fmt.Errorf("wraparound fixture %s not removed", f.table)
	}
	if err != nil {
		return err
	}
	return e.clearRunway(ctx)
}
