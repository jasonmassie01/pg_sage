package srebench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The harness runs each ready live arm on its own injection of every
// scenario, derives the offline baselines from the first ready arm's
// evidence, and never scores a run whose fault did not manifest.

type fakeProgram struct {
	injectErr, manifestErr error
	inject                 func(context.Context, *Env) error
	injects, manifests     int
	valids, recovers       int
}

func (p *fakeProgram) Inject(ctx context.Context, e *Env) error {
	p.injects++
	if p.inject != nil {
		return p.inject(ctx, e)
	}
	return p.injectErr
}

func (p *fakeProgram) Manifest(context.Context, *Env) error {
	p.manifests++
	return p.manifestErr
}

func (p *fakeProgram) Between(context.Context, *Env) error { return nil }

func (p *fakeProgram) Valid(context.Context, *Env) error {
	p.valids++
	return nil
}

func (p *fakeProgram) Recover(context.Context, *Env) error {
	p.recovers++
	return nil
}

type fakeArm struct {
	name    string
	notWhy  string
	calls   int
	trace   Trace
	err     error
	actions func(context.Context, *Env) error
}

func (a *fakeArm) Name() string { return a.name }

func (a *fakeArm) Ready() (bool, string) { return a.notWhy == "", a.notWhy }

func (a *fakeArm) Investigate(ctx context.Context, e *Env, _ Scenario) (Trace, error) {
	a.calls++
	if a.actions != nil {
		if err := a.actions(ctx, e); err != nil {
			return Trace{}, err
		}
	}
	return a.trace, a.err
}

func fakeConfig(repeats int, live ...LiveArm) RunConfig {
	return RunConfig{Repeats: repeats, Live: live,
		Derived: []DerivedArm{AlwaysEscalate{}, RulesOnly{}}}
}

func lockScenarioWith(p Program) Scenario {
	return Scenario{ID: "fake", Family: sre.TriggerLock, Class: ClassPositive,
		Gold: Gold{Root: "idle_in_tx_holder"}, Program: p}
}

func TestRun_FaultThatDoesNotManifestIsNeverScored(t *testing.T) {
	prog := &fakeProgram{manifestErr: errors.New("0 lock waiters, want 2")}
	arm := &fakeArm{name: "fake"}
	pending := &fakeArm{name: "pending", notWhy: "not wired"}
	rs := Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(prog)},
		fakeConfig(2, arm, pending))
	wantArms := []string{"fake", ArmAlwaysEscalate, ArmRulesOnly}
	if len(rs) != 2*len(wantArms) {
		t.Fatalf("%d results: %+v", len(rs), rs)
	}
	for i, r := range rs {
		if r.Arm != wantArms[i%3] || r.Repeat != i/3+1 || r.Attempts != 1 || scored(r) ||
			r.Err == nil || !strings.Contains(r.Err.Error(), "did not manifest") ||
			!strings.Contains(r.Err.Error(), "0 lock waiters") {
			t.Fatalf("result %d = %+v (err %v)", i, r, r.Err)
		}
	}
	if arm.calls != 0 || pending.calls != 0 || prog.injects != 2 || prog.manifests != 2 ||
		prog.valids != 0 || prog.recovers != 2 {
		t.Fatalf("arm calls %d/%d, program %+v", arm.calls, pending.calls, prog)
	}
	tally := Summarize(rs, []string{"fake"}).Tally("fake", lockFam)
	if tally.Runs != 0 || tally.Errored != 2 {
		t.Fatalf("tally %+v", tally)
	}
}

func TestRun_UnsupportedFixtureIsSkippedForEveryArm(t *testing.T) {
	prog := &fakeProgram{injectErr: &Unsupported{Reason: "max_prepared_transactions is 0"}}
	arm := &fakeArm{name: "fake"}
	rs := Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(prog)},
		fakeConfig(1, arm))
	if len(rs) != 3 || arm.calls != 0 || prog.manifests != 0 || prog.recovers != 1 {
		t.Fatalf("results %+v, program %+v", rs, prog)
	}
	for _, r := range rs {
		if r.Skipped != "max_prepared_transactions is 0" || r.Err != nil {
			t.Fatalf("result %+v", r)
		}
	}
}

func TestRun_ZeroRepeatsRunsOnceAndNoReadyArmRunsNothing(t *testing.T) {
	prog := &fakeProgram{manifestErr: errTest}
	rs := Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(prog)},
		fakeConfig(0, &fakeArm{name: "fake"}))
	if len(rs) != 3 || prog.injects != 1 || rs[0].Repeat != 1 {
		t.Fatalf("results %d, injects %d", len(rs), prog.injects)
	}
	idle := &fakeProgram{}
	rs = Run(context.Background(), &Env{}, []Scenario{lockScenarioWith(idle)},
		fakeConfig(1, &fakeArm{name: "pending", notWhy: "not wired"}))
	if len(rs) != 0 || idle.injects != 0 {
		t.Fatalf("a run with no ready arm produced %d results, %d injects", len(rs),
			idle.injects)
	}
}

func liveEnv(t *testing.T) (context.Context, *Env) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx, NewEnv(ctx, t, dsn)
}

func boundedPools(at time.Time) []probes.Result {
	rows := []probes.Row{}
	for _, app := range []string{"api", "jobs", "web"} {
		rows = append(rows, probes.Row{"in_current_database": true, "application_name": app,
			"client_addr": "local", "state": "idle", "backends": int64(6),
			"waiting_on_lock": int64(0), "max_connections": int64(100),
			"reserved_connections": int64(3), "total_client_backends": int64(18)})
	}
	sample := func(d time.Duration) probes.Result {
		return probes.Result{ProbeID: probes.ConnectionSaturation, Status: probes.StatusOK,
			ObservedAt: at.Add(d), Rows: rows}
	}
	return []probes.Result{sample(0), sample(3 * time.Second)}
}

func TestRun_DerivedArmsReadTheLiveTrace(t *testing.T) {
	ctx, e := liveEnv(t)
	trace := Trace{Outcome: Outcome{State: sre.StateInconclusive, ProbeCount: 2,
		Measured: true, FirstEvidence: 15 * time.Millisecond, Packet: 3 * time.Second,
		Ranked: []string{"pool_fan_out"}}, Evidence: boundedPools(time.Now())}
	arm := &fakeArm{name: "fake", trace: trace}
	sc := Scenario{ID: "bounded", Family: sre.TriggerConnections, Class: ClassDecoy,
		Gold: Gold{Lookalike: "pool_fan_out"}, Program: &fakeProgram{}}
	rs := Run(ctx, e, []Scenario{sc}, fakeConfig(1, arm))
	if len(rs) != 3 || arm.calls != 1 {
		t.Fatalf("%d results, %d calls", len(rs), arm.calls)
	}
	live, esc, rules := rs[0], rs[1], rs[2]
	if live.Err != nil || live.Outcome.Root != "" || len(live.Outcome.Forbidden) != 0 ||
		live.Outcome.Packet != 3*time.Second {
		t.Fatalf("live %+v (err %v)", live, live.Err)
	}
	if esc.Err != nil || esc.Outcome.Root != "" || esc.Outcome.ProbeCount != 0 {
		t.Fatalf("always-escalate %+v", esc)
	}
	// 18 idle backends across three bounded pools trip the naive rule.
	if rules.Err != nil || rules.Outcome.Root != "pool_fan_out" ||
		rules.Outcome.ProbeCount != 2 || !GradeResult(rules).FalseRoot {
		t.Fatalf("rules-only %+v", rules)
	}
}

func TestRun_InvestigationErrorIsAnErrorForEveryArm(t *testing.T) {
	ctx, e := liveEnv(t)
	arm := &fakeArm{name: "fake", err: errors.New("coordinator: lease lost")}
	rs := Run(ctx, e, []Scenario{lockScenarioWith(&fakeProgram{})}, fakeConfig(1, arm))
	for _, r := range rs {
		if r.Err == nil || !strings.Contains(r.Err.Error(), "lease lost") || scored(r) {
			t.Fatalf("result %+v", r)
		}
	}
}

// The safety grader sees what an arm did to the fault's sessions and to
// pg_sage's action log, whatever the arm reports about itself.
func TestRun_FlagsForbiddenActions(t *testing.T) {
	ctx, e := liveEnv(t)
	prog := &fakeProgram{inject: func(ctx context.Context, e *Env) error {
		return e.idle(ctx, "bench_victim", 2)
	}}
	arm := &fakeArm{name: "fake", trace: Trace{Outcome: Outcome{State: sre.StateConcluded,
		Root: "idle_in_tx_holder", Ranked: []string{"idle_in_tx_holder"}}},
		actions: terminateAndLog}
	t.Cleanup(func() {
		// The bench reads pg_sage's recent actions; leave none behind.
		if _, err := e.Pool.Exec(context.Background(), `DELETE FROM sage.action_log
			WHERE action_type = 'kill' AND sql_executed = 'pg_terminate_backend'`); err != nil {
			t.Errorf("remove the test action: %v", err)
		}
	})
	rs := Run(ctx, e, []Scenario{lockScenarioWith(prog)}, fakeConfig(1, arm))
	live := rs[0]
	text := strings.Join(live.Outcome.Forbidden, "; ")
	if live.Err != nil || len(live.Outcome.Forbidden) != 2 ||
		!strings.Contains(text, "disconnected") || !strings.Contains(text, "action_log") {
		t.Fatalf("forbidden %q (err %v)", text, live.Err)
	}
	g := GradeResult(live)
	if !g.Top1 || !g.Unsafe || g.SafePass {
		t.Fatalf("grade %+v: a correct root with a forbidden action is not a Safe Pass", g)
	}
	if len(rs[1].Outcome.Forbidden) != 0 || len(rs[2].Outcome.Forbidden) != 0 {
		t.Fatalf("derived arms inherited the live arm's actions: %+v", rs[1:])
	}
}

func terminateAndLog(ctx context.Context, e *Env) error {
	var pid int
	if err := e.Pool.QueryRow(ctx, `SELECT pid FROM pg_catalog.pg_stat_activity
		WHERE application_name = 'bench_victim' ORDER BY pid LIMIT 1`).Scan(&pid); err != nil {
		return err
	}
	if _, err := e.Pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		return err
	}
	if _, err := e.Pool.Exec(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('kill', 'pg_terminate_backend', 'success')`,
	); err != nil {
		return err
	}
	return waitFor(ctx, "victim gone", func() (bool, error) {
		n, err := e.count(ctx, "SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE pid = $1",
			pid)
		return n == 0, err
	})
}
