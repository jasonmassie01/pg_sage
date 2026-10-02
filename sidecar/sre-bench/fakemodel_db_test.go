package srebench

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The fake adversarial model against the real investigator (real store,
// real prompt, a scripted lock chain): the model ranks the graph's last
// hypothesis first, so on a conclusive graph the graph wins and the
// disagreement is recorded; on an inconclusive graph its probe runs.

type chainProbes struct{ graphTimeouts int }

func (c *chainProbes) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	if id != probes.LockGraph {
		return res
	}
	if c.graphTimeouts > 0 {
		c.graphTimeouts--
		res.Status, res.Reason = probes.StatusError, "statement_timeout"
		return res
	}
	edge := func(w, b int64, mode string, waiting bool) probes.Row {
		return probes.Row{"waiter_pid": w, "lock_type": "relation", "requested_mode": mode,
			"relation": "public.orders", "blocker_pid": b, "blocker_kind": "backend",
			"blocker_state": "idle in transaction", "blocker_waiting": waiting,
			"blocker_xact_age_s": 90.0, "blocker_backend_start": time.Now().UTC()}
	}
	res.Status = probes.StatusOK
	res.Rows = []probes.Row{edge(20, 4242, "AccessExclusiveLock", false),
		edge(30, 20, "AccessShareLock", true)}
	return res
}

// adversarialScenario is a scenario id whose first fake call is the
// valid adversarial reply.
func adversarialScenario(t *testing.T) string {
	t.Helper()
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("fake-%d", i)
		if FakeModeFor(id, 0) == FakeAdversarial && FakeModeFor(id, 1) == FakeAdversarial {
			return id
		}
	}
	t.Fatal("no scenario id answers adversarially twice")
	return ""
}

func fakeInvestigate(t *testing.T, timeouts int) (sre.Investigation, map[string]int) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	env := NewEnv(ctx, t, dsn)
	scID := adversarialScenario(t)
	client, _, done, err := LLMArm{Config: LLMConfig{Mode: LLMFake}}.tappedClient(
		Scenario{ID: scID})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(done)
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: env.Store,
		Runner: &chainProbes{graphTimeouts: timeouts}, Model: client,
		Notices: &sre.OnceLog{}, Config: sre.DefaultCoordinatorConfig("fake:" + scID +
			fmt.Sprint(time.Now().UnixNano()))})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	scope, err := coord.Bind(ctx)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "fake", Kind: sre.TriggerLock,
		Subject: "fake chain"})
	if err != nil || coord.Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigate: %v", err)
	}
	got, _ := env.Store.Get(ctx, scope, inv.ID)
	events, _ := env.Store.Events(ctx, scope, inv.ID)
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	return got, counts
}

func TestFakeModel_ConclusiveGraphWinsAgainstTheRealInvestigator(t *testing.T) {
	inv, events := fakeInvestigate(t, 0)
	if inv.State != sre.StateConcluded || inv.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("investigation %s root %q", inv.State, inv.Summary.Root)
	}
	if inv.ModelTurns != 1 || events[sre.EventModelDisagreed] != 1 ||
		inv.Summary.ModelRanking != nil || inv.Summary.ModelProbe != nil {
		t.Fatalf("turns %d events %v summary %+v", inv.ModelTurns, events, inv.Summary)
	}
}

func TestFakeModel_ProbeRunsOnAnInconclusiveGraph(t *testing.T) {
	inv, events := fakeInvestigate(t, 1)
	if inv.Summary.ModelProbe == nil || inv.ProbeCount != 5 || inv.ModelTurns != 2 {
		t.Fatalf("probe %+v count %d turns %d events %v", inv.Summary.ModelProbe,
			inv.ProbeCount, inv.ModelTurns, events)
	}
	if inv.State == sre.StateFailed || events[sre.EventModelRejected]+
		events[sre.EventModelDisagreed]+events[sre.EventModelReviewed] == 0 {
		t.Fatalf("state %s events %v", inv.State, events)
	}
}
