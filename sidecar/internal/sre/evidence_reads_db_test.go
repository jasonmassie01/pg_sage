package sre

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// sre-11: the investigator reads each committed read back by its step
// key and answers graph_state from a session cache, so a broad run makes
// two full evidence reads (the cache seed and finish), not two per tool
// call. With no meta database the store is the monitored database: every
// read saved is load kept off a sick server.
//
// No concurrency test here: StepEvidence and StepKeys are single reads;
// two workers on one investigation are fenced by the lease (lockLeased),
// which the store's fence tests cover.

// tracedStore is a live store whose pool records every statement.
func tracedStore(t *testing.T) (*PostgresStore, *pgxpool.Pool, *testdb.QueryRecorder,
	context.Context) {
	t.Helper()
	_, pool, ctx := liveStore(t, budgetLimits())
	rec := &testdb.QueryRecorder{}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.Tracer = rec
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	st, err := NewPostgresStore(traced, budgetLimits())
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	return st, pool, rec, ctx
}

func nilArg(a any) bool {
	if a == nil {
		return true
	}
	v := reflect.ValueOf(a)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// evidenceReads counts the evidence queries (every row with its payload)
// that name neither one evidence id nor one step key (full), and those
// that name one step key (step).
func evidenceReads(rec *testdb.QueryRecorder) (full, step int) {
	for _, q := range rec.Matching("FROM sage.sre_evidence e", "e.payload::text") {
		byID := len(q.Args) > 3 && !nilArg(q.Args[3])
		byKey := len(q.Args) > 4 && !nilArg(q.Args[4])
		switch {
		case byKey:
			step++
		case !byID:
			full++
		}
	}
	return full, step
}

func operatorTrigger(subject string) Trigger {
	return Trigger{CaseID: "operator:" + subject, Kind: TriggerOperator,
		Subject: subject + " is slow", Actor: "user:1"}
}

// broadReads is a broad run of 6 reads, each followed by graph_state.
// The loop refuses a repeated identical call, so each graph_state call
// carries different (ignored) arguments, as a model may send them.
func broadReads() []fakeReply {
	var out []fakeReply
	for _, id := range []probes.ID{probes.LockGraph, probes.LongTransactions,
		probes.PreparedXacts, probes.ConnectionSaturation, probes.PlanRegressions,
		probes.ReplicationSlots} {
		out = append(out, toolCalls(ToolRunProbe, probeArgs(id), ToolGraphState,
			`{"after":"`+string(id)+`"}`))
	}
	return append(out, submitFixed(invFinal{Outcome: "inconclusive"}))
}

type countedRun struct {
	full, step int
	inv        Investigation
}

// countReads runs one operator triage: with no replies there is no model
// (no investigator session at all).
func countReads(t *testing.T, replies ...fakeReply) countedRun {
	t.Helper()
	st, _, rec, ctx := tracedStore(t)
	var c *Coordinator
	if len(replies) == 0 {
		c, _ = testCoordinator(t, ctx, st, idleChainRunner(), nil)
	} else {
		m := newFakeModel(t, replies...)
		c, _ = investigatorCoordinator(t, ctx, st, idleChainRunner(), m.client(),
			invOptions{})
	}
	inv := startAndRun(t, ctx, c, operatorTrigger("checkout-"+string(NewUUID())))
	if inv.State != StateConcluded || inv.Summary.Root != "idle_in_tx_holder" {
		t.Fatalf("operator triage = %s root %q (%s), want the idle holder", inv.State,
			inv.Summary.Root, inv.FailureCode)
	}
	full, step := evidenceReads(rec)
	return countedRun{full: full, step: step, inv: inv}
}

func TestEvidenceReads_BroadRunReadsAllEvidenceTwice(t *testing.T) {
	none := countReads(t)
	quiet := countReads(t, submitFixed(invFinal{Outcome: "inconclusive"}))
	broad := countReads(t, broadReads()...)
	run := transcriptOf(t, broad.inv)
	reads, graphs := stepsOf(run, ToolRunProbe), stepsOf(run, ToolGraphState)
	if run.Plan != PlanBroad || len(reads) != 6 || len(graphs) != 6 {
		t.Fatalf("plan %s, %d reads, %d graph_state calls; want the broad plan, 6 and 6",
			run.Plan, len(reads), len(graphs))
	}
	for _, r := range reads {
		if r.EvidenceID == "" || r.Status == "" {
			t.Fatalf("read step %+v carries no stored evidence", r)
		}
	}
	for _, g := range graphs {
		if g.Status != "ok" {
			t.Fatalf("graph_state step %+v did not run", g)
		}
	}
	// A session that never asks for the graph reads all evidence once, at
	// finish; one that does seeds its cache once more, and no tool call
	// adds a full read.
	if got := quiet.full - none.full; got != 1 {
		t.Errorf("a session without graph_state made %d full evidence reads, want 1", got)
	}
	if got := broad.full - none.full; got != 2 {
		t.Errorf("a broad session of 6 reads made %d full evidence reads, want 2 "+
			"(the cache seed and finish)", got)
	}
	if broad.step != 6 || quiet.step != 0 || none.step != 0 {
		t.Errorf("step reads = %d broad, %d quiet, %d none; want one per committed read",
			broad.step, quiet.step, none.step)
	}
}

// claimedEvaluating creates and claims an investigation and moves it to
// evaluating, as collect leaves it for the investigator.
func claimedEvaluating(t *testing.T, ctx context.Context, st *PostgresStore,
	req StartRequest) (Investigation, Lease) {
	t.Helper()
	inv, _, err := st.Create(ctx, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	lease, err := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.CommitStep(ctx, lease, step("evaluate-f1", StateEvaluating)); err != nil {
		t.Fatalf("move to evaluating: %v", err)
	}
	return inv, lease
}

func TestEvidenceReads_StepEvidenceReturnsOneStepsRows(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 1101"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	one := probeResult(probes.LongTransactions, probes.Row{"pid": int64(5)})
	two := []probes.Result{probeResult(probes.LockGraph, probes.Row{"pid": int64(6)}),
		probeResult(probes.PreparedXacts)}
	if _, err := st.CommitStep(ctx, lease, step("a", StateCollecting, one)); err != nil {
		t.Fatalf("commit a: %v", err)
	}
	if _, err := st.CommitStep(ctx, lease, step("b", StateCollecting, two...)); err != nil {
		t.Fatalf("commit b: %v", err)
	}
	all, err := st.Evidence(ctx, inv.Scope, inv.ID)
	if err != nil || len(all) != 3 {
		t.Fatalf("evidence = %d rows (%v), want 3", len(all), err)
	}
	for _, key := range []string{"a", "b"} {
		got, err := st.StepEvidence(ctx, inv.Scope, inv.ID, key)
		if err != nil {
			t.Fatalf("StepEvidence(%s): %v", key, err)
		}
		var want []Evidence
		for _, e := range all {
			if e.StepKey == key {
				want = append(want, e)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("StepEvidence(%s) = %+v, want the full read's rows %+v", key, got, want)
		}
		for _, e := range got {
			if !e.VerifyHash() || len(e.Payload) == 0 {
				t.Fatalf("StepEvidence(%s) row %s: payload %q does not verify", key, e.ID,
					e.Payload)
			}
		}
	}
}

func TestEvidenceReads_StepEvidenceOfAnUnknownStepIsNotFound(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, lease := claimedEvaluating(t, ctx, st, lockStart(testScope(t, ctx, st), "pid 1102"))
	if _, err := st.CommitStep(ctx, lease, step("read", StateNeedsEvidence,
		probeResult(probes.LockGraph))); err != nil {
		t.Fatalf("commit: %v", err)
	}
	for name, key := range map[string]string{"unknown key": "never-committed",
		"step without evidence": "evaluate-f1"} {
		if got, err := st.StepEvidence(ctx, inv.Scope, inv.ID, key); !errors.Is(err,
			ErrNotFound) || got != nil {
			t.Errorf("%s: StepEvidence = %+v (%v), want ErrNotFound", name, got, err)
		}
	}
	other := testScope(t, ctx, st)
	if got, err := st.StepEvidence(ctx, other, inv.ID, "read"); !errors.Is(err,
		ErrNotFound) || got != nil {
		t.Errorf("another database's scope: StepEvidence = %+v (%v), want ErrNotFound",
			got, err)
	}
}

func TestEvidenceReads_StepEvidenceRefusesInvalidKeys(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 1103"))
	for name, key := range map[string]string{"empty": "",
		"161 bytes": strings.Repeat("k", 161), "control character": "inv\nread"} {
		if _, err := st.StepEvidence(ctx, inv.Scope, inv.ID, key); !errors.Is(err,
			ErrInvalidRequest) {
			t.Errorf("%s key: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	// 160 bytes is the longest key CommitStep stores: it is a lookup.
	if _, err := st.StepEvidence(ctx, inv.Scope, inv.ID,
		strings.Repeat("k", 160)); !errors.Is(err, ErrNotFound) {
		t.Errorf("160-byte key: err = %v, want ErrNotFound", err)
	}
	if _, err := st.StepEvidence(ctx, inv.Scope, "not-a-uuid", "read"); !errors.Is(err,
		ErrInvalidRequest) {
		t.Errorf("invalid investigation id: err = %v, want ErrInvalidRequest", err)
	}
	if _, err := st.StepKeys(ctx, Scope{}, inv.ID); err == nil {
		t.Error("StepKeys with an empty scope succeeded")
	}
}

func TestEvidenceReads_StepKeysListsTheKeysOfStoredReads(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 1104"))
	keys, err := st.StepKeys(ctx, inv.Scope, inv.ID)
	if err != nil || keys == nil || len(keys) != 0 {
		t.Fatalf("StepKeys of a new investigation = %v (nil %v, %v), want empty, non-nil",
			keys, keys == nil, err)
	}
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	r := probeResult(probes.LockGraph)
	for _, s := range []StepResult{step("step-1", StateCollecting, r, r),
		step("step-1", StateCollecting, r), step("step-2", StateCollecting, r),
		step("evaluate-f1", StateEvaluating)} {
		if _, err := st.CommitStep(ctx, lease, s); err != nil {
			t.Fatalf("commit %s: %v", s.IdempotencyKey, err)
		}
	}
	keys, err = st.StepKeys(ctx, inv.Scope, inv.ID)
	// As collect read them from the evidence before: the steps that stored
	// a read, once each.
	want := map[string]bool{"step-1": true, "step-2": true}
	if err != nil || !reflect.DeepEqual(keys, want) {
		t.Fatalf("StepKeys = %v (%v), want %v", keys, err, want)
	}
}

func TestEvidenceReads_RetriedStepReturnsTheOriginalRowOnce(t *testing.T) {
	st, _, ctx := liveStore(t, DefaultLimits())
	inv, _, _ := st.Create(ctx, lockStart(testScope(t, ctx, st), "pid 1105"))
	lease, _ := st.Claim(ctx, inv.Scope, inv.ID, NewUUID())
	first := probeResult(probes.LongTransactions, probes.Row{"pid": int64(7)})
	retry := probeResult(probes.LongTransactions, probes.Row{"pid": int64(8)})
	for _, res := range []probes.Result{first, retry} {
		if _, err := st.CommitStep(ctx, lease, step("same", StateCollecting,
			res)); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	got, err := st.StepEvidence(ctx, inv.Scope, inv.ID, "same")
	if err != nil || len(got) != 1 {
		t.Fatalf("StepEvidence = %d rows (%v), want the original row once", len(got), err)
	}
	if !strings.Contains(string(got[0].Payload), `"pid": 7`) &&
		!strings.Contains(string(got[0].Payload), `"pid":7`) {
		t.Fatalf("payload %s is not the original read (pid 7)", got[0].Payload)
	}
}
