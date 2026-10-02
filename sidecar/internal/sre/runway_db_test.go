package sre

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Runway investigations through the real coordinator and store: the
// conclusion carries the custodian proposals the advisor returns (with
// the gate's verdict) for a conclusive runway diagnosis only, an advisor
// failure never fails the investigation, and the plan runs against real
// PostgreSQL.

type fakeAdvisor struct {
	mu    sync.Mutex
	calls []AdviceRequest
	out   []ActionProposal
	err   error
}

func (f *fakeAdvisor) Advise(_ context.Context, req AdviceRequest) ([]ActionProposal,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return f.out, f.err
}

func (f *fakeAdvisor) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func seqRow(last float64) probes.Row {
	return probes.Row{"sequence": "public.orders_id_seq", "data_type": "bigint",
		"increment_by": int64(1), "cycle": false, "last_value": last,
		"min_value": int64(1), "max_value": int64(9223372036854775807),
		"type_max": int64(9223372036854775807), "owner_column": "public.orders.id",
		"owner_type": "integer", "owner_type_max": int64(2147483647),
		"effective_limit": int64(2147483647), "fraction_used": last / 2147483647}
}

// narrowColumnRunner scripts a bigint sequence owned by an integer column,
// consuming 500 values between the two samples.
func narrowColumnRunner() *scriptedRunner {
	return newScriptedRunner().script(probes.SequenceRunwayProbe,
		rows(probes.SequenceRunwayProbe, seqRow(2e9)),
		rows(probes.SequenceRunwayProbe, seqRow(2e9+500)))
}

func seqTrigger() Trigger {
	return Trigger{CaseID: "finding:db:forecast_sequence_runway:sequence:" +
		"public.orders_id_seq", Kind: TriggerSequence,
		Subject: "sequence public.orders_id_seq", IdempotencyKey: "runway:1:warning"}
}

func advisedCoordinator(t *testing.T, ctx context.Context, st *PostgresStore,
	runner ProbeRunner, adv ActionAdvisor) *Coordinator {
	t.Helper()
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	c.advisor = adv
	return c
}

func TestCoordinator_RunwayConclusionCarriesCustodianProposals(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	adv := &fakeAdvisor{out: []ActionProposal{{Feature: "sequence",
		Action:  "widen public.orders.id to bigint (manual migration)",
		Targets: []string{"public.orders.id"}, Verdict: "manual_only"}}}
	c := advisedCoordinator(t, ctx, st, narrowColumnRunner(), adv)
	inv := startAndRun(t, ctx, c, seqTrigger())
	if inv.State != StateConcluded ||
		inv.Summary.Root != "column_narrower_than_sequence" {
		t.Fatalf("investigation = %s root %q (%s)", inv.State, inv.Summary.Root,
			inv.Summary.Reason)
	}
	if adv.count() != 1 || adv.calls[0].Kind != TriggerSequence ||
		adv.calls[0].Root != "column_narrower_than_sequence" ||
		adv.calls[0].Subject != "sequence public.orders_id_seq" {
		t.Fatalf("advisor calls = %+v", adv.calls)
	}
	if len(inv.Summary.Proposals) != 1 || inv.Summary.Proposals[0].Verdict != "manual_only" {
		t.Fatalf("proposals = %+v", inv.Summary.Proposals)
	}
	if inv.Summary.Family != "sequence_runway" {
		t.Fatalf("family = %q", inv.Summary.Family)
	}
}

// The advisor is consulted only for a conclusive runway diagnosis: never
// for an R1 family, never for an inconclusive runway.
func TestCoordinator_AdvisorOnlyForConclusiveRunways(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	adv := &fakeAdvisor{out: []ActionProposal{validProposal()}}
	c := advisedCoordinator(t, ctx, st, idleChainRunner(), adv)
	lock := startAndRun(t, ctx, c, lockTrigger("inc-adv"))
	dormant := newScriptedRunner().script(probes.SequenceRunwayProbe,
		rows(probes.SequenceRunwayProbe, seqRow(10)))
	c2 := advisedCoordinator(t, ctx, st, dormant, adv)
	seq := startAndRun(t, ctx, c2, seqTrigger())
	if adv.count() != 0 {
		t.Fatalf("advisor consulted %d times: %+v", adv.count(), adv.calls)
	}
	if len(lock.Summary.Proposals)+len(seq.Summary.Proposals) != 0 ||
		seq.State != StateInconclusive {
		t.Fatalf("lock %+v seq %s %+v", lock.Summary.Proposals, seq.State,
			seq.Summary.Proposals)
	}
}

// Error propagation: an advisor failure (or invalid proposals) leaves the
// deterministic conclusion intact and is logged.
func TestCoordinator_AdvisorFailureKeepsTheConclusion(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	var mu sync.Mutex
	var logs []string
	for name, adv := range map[string]*fakeAdvisor{
		"error":   {err: errors.New("custodian scan failed")},
		"invalid": {out: []ActionProposal{{Feature: "freeze"}}},
	} {
		c := advisedCoordinator(t, ctx, st, narrowColumnRunner(), adv)
		c.logFn = func(level, msg string, args ...any) {
			mu.Lock()
			logs = append(logs, level+" "+fmt.Sprintf(msg, args...))
			mu.Unlock()
		}
		inv := startAndRun(t, ctx, c, Trigger{CaseID: "case:" + name,
			Kind: TriggerSequence, Subject: "sequence public.orders_id_seq",
			IdempotencyKey: "runway:" + name})
		if inv.State != StateConcluded || len(inv.Summary.Proposals) != 0 {
			t.Fatalf("%s: state %s proposals %+v", name, inv.State, inv.Summary.Proposals)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "custodian scan failed") ||
		!strings.Contains(joined, "proposal") {
		t.Fatalf("logs = %s, want both advisor failures logged", joined)
	}
}

// The sequence plan against real PostgreSQL: a bigint sequence owned by
// an integer column, consumed between the samples.
func TestCoordinator_RunwayInvestigationOnRealPostgres(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	name := fmt.Sprintf("sre_runway_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SEQUENCE %[1]s_seq;
		CREATE TABLE %[1]s (id int DEFAULT nextval('%[1]s_seq'));
		ALTER SEQUENCE %[1]s_seq OWNED BY %[1]s.id;
		SELECT setval('%[1]s_seq', 2147483647 - 100000)`, name)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+name) })
	runner := probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1))
	c, _ := testCoordinator(t, ctx, st, runner, nil)
	c.sleep = func(ctx context.Context, _ time.Duration) error {
		_, err := pool.Exec(ctx, "INSERT INTO "+name+" SELECT FROM generate_series(1, 300)")
		return err
	}
	inv := startAndRun(t, ctx, c, Trigger{CaseID: "case:" + name, Kind: TriggerSequence,
		Subject: "sequence public." + name + "_seq", IdempotencyKey: "runway:" + name})
	if inv.State != StateConcluded ||
		inv.Summary.Root != "column_narrower_than_sequence" || inv.ProbeCount != 4 {
		t.Fatalf("investigation = %s root %q probes %d (%s)", inv.State, inv.Summary.Root,
			inv.ProbeCount, inv.Summary.Reason)
	}
}
