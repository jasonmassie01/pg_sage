package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/leader"
	"github.com/pg-sage/sidecar/internal/siem"
)

// recordingSink counts what the exporter delivers.
type recordingSink struct {
	mu     sync.Mutex
	events int
}

func (s *recordingSink) Name() string { return "reload-soc" }

func (s *recordingSink) Send(_ context.Context, events []siem.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events += len(events)
	return nil
}

func (s *recordingSink) delivered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events
}

// reloadSIEMExporter exports the running fleet's audit chains the way
// startSIEMExport does, fenced by auditLeaderFence.
func reloadSIEMExporter(control *pgxpool.Pool, sink siem.Sink) *siem.Exporter {
	sources := auditSources(control, fleetMgr)
	return siem.NewExporter(func() []siem.Source {
		var out []siem.Source
		for _, s := range sources() {
			out = append(out, siem.Source{Name: s.Name, DB: s.Pool})
		}
		return out
	}, []siem.SinkConfig{{Sink: sink}}, siem.NewCursorStore(control),
		siem.Options{BatchSize: 50}, auditLeaderFence, nil)
}

func addReloadAction(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	_, err := p.Exec(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome)
		VALUES ('create_index', 'CREATE INDEX i ON t (a)', 'success')`)
	if err != nil {
		t.Fatalf("add action: %v", err)
	}
}

// A YAML reload that adds a database changes the fleet's database list
// but not the lease the leader holds: SIEM export keeps passing its fence
// and delivers new links (fleet-02). When the lease moves to another
// holder, the pass stops with ErrFenced and the sink status says so.
func TestFleetReloadKeepsSIEMFence(t *testing.T) {
	ctx := context.Background()
	preserveAuditFenceGlobals(t) // restored after the runtimes drain (env-race)
	env := newFleetReloadEnv(t, "ctl", "b")
	control := fleetMgr.GetInstance("ctl").Pool
	scope := fleetLearningScope(cfg)
	setFleetLeader(leader.NewElector(leader.NewPostgresStore(control), scope,
		"reload-holder", time.Minute))
	if err := currentFleetLeader().Tick(ctx); err != nil || !currentFleetLeader().IsLeader() {
		t.Fatalf("take the lease: %v (leader %v)", err, currentFleetLeader().IsLeader())
	}
	sink := &recordingSink{}
	exp := reloadSIEMExporter(control, sink)
	addReloadAction(t, control)
	if err := exp.RunOnce(ctx); err != nil || sink.delivered() == 0 {
		t.Fatalf("export before reload: err %v, delivered %d", err, sink.delivered())
	}
	extra := testdbExtra(t, env, "c")
	if err := env.reload(func(c *config.Config) {
		c.Databases = append(c.Databases, extra)
	}); err != nil {
		t.Fatalf("add reload: %v", err)
	}
	if fleetLearningScope(cfg) == scope {
		t.Fatal("fixture: the reloaded fleet must hash to another scope")
	}
	before := sink.delivered()
	addReloadAction(t, control)
	if err := exp.RunOnce(ctx); err != nil {
		t.Fatalf("export after reload: %v", err)
	}
	if st := exp.Status()[0]; sink.delivered() <= before || st.ConsecutiveFailures != 0 {
		t.Fatalf("after reload: delivered %d (was %d), status %+v",
			sink.delivered(), before, st)
	}
	assertSIEMFencedAfterTakeover(t, exp, control, scope)
}

// assertSIEMFencedAfterTakeover hands the lease to another sidecar while
// this one still believes it leads, and checks export refuses.
func assertSIEMFencedAfterTakeover(t *testing.T, exp *siem.Exporter,
	control *pgxpool.Pool, scope string) {
	t.Helper()
	ctx := context.Background()
	tag, err := control.Exec(ctx, `UPDATE sage.fleet_leader_lease
		SET holder = 'other-sidecar', epoch = epoch + 1 WHERE scope = $1`, scope)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("move the lease: %v (rows %d)", err, tag.RowsAffected())
	}
	if err := exp.RunOnce(ctx); !errors.Is(err, siem.ErrFenced) {
		t.Fatalf("export after takeover: err = %v, want ErrFenced", err)
	}
	st := exp.Status()[0]
	if st.ConsecutiveFailures != 1 || !strings.Contains(st.LastError, "leader lease lost") {
		t.Fatalf("status after takeover = %+v, want one fenced failure", st)
	}
}
